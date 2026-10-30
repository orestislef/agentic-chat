// Package client is the part that runs next to a coding session: the stdio bridge,
// the hook commands and the setup command.
package client

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/orestislef/agentic-chat/internal/mcp"
)

const (
	// channelMethod is the notification that injects a message into a running session.
	channelMethod = "notifications/claude/channel"

	maxLineBytes  = 32 << 20
	maxFrameBytes = 1 << 20

	// attemptTimeout is longer than the longest blocking tool call on the hub.
	attemptTimeout = 125 * time.Second
	// toolBudget covers one full wait plus a hub restart along the way.
	toolBudget = 4 * time.Minute

	pingEvery   = 30 * time.Second
	pingTimeout = 15 * time.Second

	// Close codes sent by the hub, see internal/server.
	closeSuperseded = websocket.StatusCode(4001)
	closeRevoked    = websocket.StatusCode(4003)
)

// Config says which hub to talk to and as whom.
type Config struct {
	URL   string
	Token string
}

// Validate normalises the URL and checks both values are there.
func (c *Config) Validate() error {
	c.URL = strings.TrimRight(strings.TrimSpace(c.URL), "/")
	c.Token = strings.TrimSpace(c.Token)
	if c.URL == "" || c.Token == "" {
		return errors.New("hub address and token are required, set AGENTIC_CHAT_URL and AGENTIC_CHAT_TOKEN or pass --url and --token")
	}
	if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
		return fmt.Errorf("hub address must start with http:// or https://, got %q", c.URL)
	}
	return nil
}

// Bridge speaks MCP over stdio to the session and forwards tool calls to the hub.
// It adds what the plain HTTP endpoint cannot do: messages pushed into the session.
type Bridge struct {
	cfg  Config
	in   io.Reader
	out  io.Writer
	log  *slog.Logger
	http *http.Client

	writeMu sync.Mutex

	pushOnce sync.Once

	callsMu sync.Mutex
	calls   map[string]context.CancelFunc

	seen *seenSet

	// Tunable so tests do not have to wait for real timeouts.
	retryDelays []time.Duration
	pingEvery   time.Duration
	pingTimeout time.Duration
}

// NewBridge creates a bridge over the given streams.
func NewBridge(cfg Config, in io.Reader, out io.Writer, log *slog.Logger) *Bridge {
	return &Bridge{
		cfg:   cfg,
		in:    in,
		out:   out,
		log:   log,
		http:  &http.Client{},
		calls: map[string]context.CancelFunc{},
		seen:  newSeenSet(1024),

		retryDelays: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 30 * time.Second},
		pingEvery:   pingEvery,
		pingTimeout: pingTimeout,
	}
}

// Run serves until stdin closes, which is how a session ends its servers.
func (b *Bridge) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	defer wg.Wait()

	scanner := bufio.NewScanner(b.in)
	scanner.Buffer(make([]byte, 64*1024), maxLineBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req mcp.Request
		if err := json.Unmarshal(line, &req); err != nil {
			b.write(mcp.Error(nil, mcp.CodeParseError, "invalid JSON"))
			continue
		}
		if req.Method == "" {
			continue // a response to something we sent, we never expect any
		}
		b.handle(ctx, &req, &wg)
	}
	cancel()
	return scanner.Err()
}

func (b *Bridge) handle(ctx context.Context, req *mcp.Request, wg *sync.WaitGroup) {
	switch req.Method {
	case "initialize":
		caps := map[string]any{
			"tools":        map[string]any{},
			"experimental": map[string]any{"claude/channel": map[string]any{}},
		}
		b.write(mcp.Result(req.ID, mcp.InitializeResult(req.Params, caps, mcp.SharedInstructions+mcp.PushInstructions)))
	case "notifications/initialized":
		b.startPush(ctx, wg)
	case "notifications/cancelled":
		b.cancelCall(req.Params)
	case "ping":
		b.write(mcp.Result(req.ID, map[string]any{}))
	case "tools/list":
		b.write(mcp.Result(req.ID, map[string]any{"tools": mcp.Tools()}))
		b.startPush(ctx, wg) // in case a client lists tools without sending initialized
	case "tools/call":
		// Asks block for up to two minutes, others must keep flowing meanwhile.
		copied := *req
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.forward(ctx, &copied)
		}()
	default:
		if !req.IsNotification() {
			b.write(mcp.Error(req.ID, mcp.CodeMethodNotFound, "method not found: "+req.Method))
		}
	}
}

func (b *Bridge) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		b.log.Error("encode", "err", err)
		return
	}
	b.writeLine(data)
}

func (b *Bridge) writeLine(data []byte) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	data = append(bytes.TrimRight(data, "\r\n"), '\n')
	if _, err := b.out.Write(data); err != nil {
		b.log.Error("write to session", "err", err)
	}
}

// forward sends a tool call to the hub and passes the answer through unchanged.
// Lost connections and a restarting hub are retried with the same idempotency key,
// so the session never sees them and nothing gets stored twice. Failures that
// remain become tool errors with a next step, a protocol error would only tell the
// model that something is broken.
func (b *Bridge) forward(ctx context.Context, req *mcp.Request) {
	ctx, cancel := context.WithTimeout(ctx, toolBudget)
	defer cancel()
	id := string(req.ID)
	b.callsMu.Lock()
	b.calls[id] = cancel
	b.callsMu.Unlock()
	defer func() {
		b.callsMu.Lock()
		delete(b.calls, id)
		b.callsMu.Unlock()
	}()

	fail := func(message string) {
		b.write(mcp.Result(req.ID, mcp.TextResult(message, true)))
	}
	body, err := json.Marshal(req)
	if err != nil {
		fail("could not encode the request")
		return
	}
	key, err := newKey()
	if err != nil {
		fail("could not generate a request key")
		return
	}

	for attempt := 0; ; attempt++ {
		data, status, err := b.post(ctx, body, key)
		if errors.Is(ctx.Err(), context.Canceled) {
			return // the session cancelled this call and expects no answer
		}
		switch {
		case err == nil && status == http.StatusOK && json.Valid(data):
			b.writeLine(bytes.ReplaceAll(data, []byte("\n"), nil))
			return
		case status == http.StatusUnauthorized:
			fail("the hub rejected this agent's token, it was probably revoked or rotated. Tell your user to create a new one in the hub's web UI.")
			return
		case status == http.StatusRequestEntityTooLarge:
			fail("that message is too large for the hub, send a shorter summary")
			return
		case status >= 400 && status < 500:
			fail(fmt.Sprintf("the hub answered with status %d", status))
			return
		}
		// Connection trouble, a 5xx from the hub or a proxy in front of it, or a cut off body.
		if attempt >= len(b.retryDelays) || ctx.Err() != nil {
			b.log.Warn("giving up on tool call", "err", err, "status", status)
			fail(fmt.Sprintf("could not reach the hub at %s. It may be down or this machine may be offline. A message you were sending may or may not have arrived, check get_history before sending it again.", b.cfg.URL))
			return
		}
		b.log.Info("retrying tool call", "attempt", attempt+1, "err", err, "status", status)
		select {
		case <-time.After(b.retryDelays[attempt]):
		case <-ctx.Done():
		}
	}
}

// post makes one attempt. A status of zero means there was no usable HTTP response.
func (b *Bridge) post(ctx context.Context, body []byte, key string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.cfg.Token)
	req.Header.Set(mcp.IdempotencyHeader, key)
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxLineBytes))
	if err != nil {
		return nil, 0, err
	}
	return data, resp.StatusCode, nil
}

func newKey() (string, error) {
	buf := make([]byte, 16)
	if _, err := crand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (b *Bridge) cancelCall(params json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	b.callsMu.Lock()
	cancel := b.calls[string(p.RequestID)]
	b.callsMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (b *Bridge) startPush(ctx context.Context, wg *sync.WaitGroup) {
	b.pushOnce.Do(func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.pushLoop(ctx)
		}()
	})
}

// pushLoop keeps the websocket to the hub open, reconnecting until the session
// ends or the hub says this connection should stay closed.
func (b *Bridge) pushLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		connected, err := b.pushOnceConnected(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errTokenRejected) {
			b.notifySystem("The hub rejected this agent's token, so this session receives no messages. Tell your user to set up the agent again.")
			return
		}
		switch websocket.CloseStatus(err) {
		case closeSuperseded:
			b.notifySystem("Another session connected with this agent's token, so this session no longer receives messages. The tools still work. Tell your user if that is not what they intended.")
			return
		case closeRevoked:
			b.notifySystem("This agent's token was revoked or rotated, so this session no longer receives messages. Tell your user to set up the agent again.")
			return
		}
		if connected {
			backoff = time.Second
		}
		b.log.Warn("hub connection lost, retrying", "err", err, "in", backoff)
		sleep := backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (b *Bridge) pushOnceConnected(ctx context.Context) (bool, error) {
	wsURL := "ws" + strings.TrimPrefix(b.cfg.URL, "http") + "/api/agent/ws"
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + b.cfg.Token}},
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return false, errTokenRejected
		}
		return false, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxFrameBytes)
	b.log.Info("connected to hub", "url", b.cfg.URL)

	// A cable pulled or a laptop lid closed gives no close signal, a read would just
	// wait forever. Unanswered pings are the only way to notice from this side.
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		ticker := time.NewTicker(b.pingEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				pingCtx, cancel := context.WithTimeout(ctx, b.pingTimeout)
				err := conn.Ping(pingCtx)
				cancel()
				if err != nil {
					conn.CloseNow()
					return
				}
			case <-watchdogDone:
				return
			}
		}
	}()

	for {
		var frame struct {
			Type    string `json:"type"`
			Message *struct {
				ID        int64  `json:"id"`
				ThreadID  int64  `json:"thread_id"`
				From      string `json:"from"`
				Kind      string `json:"kind"`
				Body      string `json:"body"`
				InReplyTo int64  `json:"in_reply_to"`
			} `json:"message"`
		}
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			return true, err
		}
		if frame.Type != "message" || frame.Message == nil {
			continue
		}
		m := frame.Message
		if b.seen.Add(m.ID) {
			meta := map[string]string{
				"from":       m.From,
				"kind":       m.Kind,
				"message_id": strconv.FormatInt(m.ID, 10),
				"thread_id":  strconv.FormatInt(m.ThreadID, 10),
			}
			if m.InReplyTo != 0 {
				meta["in_reply_to"] = strconv.FormatInt(m.InReplyTo, 10)
			}
			b.notify(sanitizeBody(m.Body), meta)
		}
		if err := wsjson.Write(ctx, conn, map[string]any{"type": "ack", "id": m.ID}); err != nil {
			return true, err
		}
	}
}

func (b *Bridge) notify(content string, meta map[string]string) {
	b.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  channelMethod,
		"params":  map[string]any{"content": content, "meta": meta},
	})
}

// notifySystem tells the session something from the bridge itself. The kind is
// one no agent can use, so it cannot be mistaken for a peer's message.
func (b *Bridge) notifySystem(text string) {
	b.notify(text, map[string]string{"from": "agentic-chat", "kind": "system"})
}

var errTokenRejected = errors.New("token rejected")

var channelTagRE = regexp.MustCompile(`(?i)<(/?channel)`)

// sanitizeBody stops a message from closing its own tag and forging a new one
// with attributes that look as if the hub set them.
func sanitizeBody(body string) string {
	return channelTagRE.ReplaceAllString(body, "&lt;${1}")
}

// seenSet remembers recent message ids, because a reconnect can deliver one twice.
type seenSet struct {
	limit int
	ids   map[int64]struct{}
	order []int64
}

func newSeenSet(limit int) *seenSet {
	return &seenSet{limit: limit, ids: map[int64]struct{}{}}
}

// Add returns true the first time it sees an id.
func (s *seenSet) Add(id int64) bool {
	if _, ok := s.ids[id]; ok {
		return false
	}
	s.ids[id] = struct{}{}
	s.order = append(s.order, id)
	if len(s.order) > s.limit {
		delete(s.ids, s.order[0])
		s.order = s.order[1:]
	}
	return true
}
