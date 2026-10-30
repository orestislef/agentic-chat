package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orestislef/agentic-chat/internal/auth"
	"github.com/orestislef/agentic-chat/internal/hub"
	"github.com/orestislef/agentic-chat/internal/server"
	"github.com/orestislef/agentic-chat/internal/store"
)

// These tests break the connection on purpose, the way real networks do.

type world struct {
	t           *testing.T
	store       *store.Store
	dir         string
	front, back string
	backendID   int64
	frontendID  int64
}

func newWorld(t *testing.T) *world {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "world.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	u, _ := s.CreateAccount(ctx, "owner", "h", "", "team")
	ws, _ := s.WorkspacesForUser(ctx, u.ID)
	w := &world{t: t, store: s, dir: dir}
	w.front, _ = auth.NewToken("ac")
	w.back, _ = auth.NewToken("ac")
	a, err := s.CreateAgent(ctx, ws[0].ID, u.ID, "frontend", "", auth.HashToken(w.front))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateAgent(ctx, ws[0].ID, u.ID, "backend", "", auth.HashToken(w.back))
	if err != nil {
		t.Fatal(err)
	}
	w.frontendID, w.backendID = a.ID, b.ID
	return w
}

func (w *world) handler() http.Handler {
	cfg := server.Config{DataDir: w.dir, DownloadsDir: w.dir, Registration: server.RegistrationInvite, Limits: hub.DefaultLimits()}
	return server.New(cfg, w.store, quietLog()).Handler()
}

// messagesFrom counts what an agent has really stored, which is what duplicates show up in.
func (w *world) messagesFrom(agentID int64) int {
	w.t.Helper()
	history, err := w.store.History(context.Background(), 1, store.HistoryFilter{AgentID: agentID, Limit: 500})
	if err != nil {
		w.t.Fatal(err)
	}
	n := 0
	for _, m := range history {
		if m.FromID == agentID {
			n++
		}
	}
	return n
}

func fastSession(t *testing.T, url, token string) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &session{t: t, stdin: inW, lines: make(chan map[string]any, 64), done: make(chan struct{})}
	bridge := NewBridge(Config{URL: url, Token: token}, inR, outW, quietLog())
	bridge.retryDelays = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond}
	bridge.pingEvery = 100 * time.Millisecond
	bridge.pingTimeout = 300 * time.Millisecond
	bridge.pushDelay = 0
	bridge.standby = 100 * time.Millisecond
	go func() {
		bridge.Run(context.Background())
		close(s.done)
	}()
	go readLines(t, outR, s.lines)
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			t.Error("bridge did not exit when stdin closed")
		}
		outW.Close()
	})
	return s
}

func readLines(t *testing.T, r io.Reader, lines chan<- map[string]any) {
	dec := json.NewDecoder(r)
	for {
		var msg map[string]any
		if err := dec.Decode(&msg); err != nil {
			return
		}
		lines <- msg
	}
}

func (s *session) nextPush(timeout time.Duration) map[string]any {
	s.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case m := <-s.lines:
			if m["method"] == channelMethod {
				return m["params"].(map[string]any)
			}
		case <-deadline:
			s.t.Fatal("no push arrived")
			return nil
		}
	}
}

// dropFirst loses the response of the first matching call after the hub has done
// the work, which is the worst moment for a connection to die.
func dropFirst(next http.Handler, tool string) http.Handler {
	var dropped atomic.Bool
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" || dropped.Load() {
			next.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		if !strings.Contains(string(body), `"`+tool+`"`) || !dropped.CompareAndSwap(false, true) {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(httptest.NewRecorder(), r)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
}

func TestLostResponseDoesNotDuplicateTheMessage(t *testing.T) {
	w := newWorld(t)
	srv := httptest.NewServer(dropFirst(w.handler(), "send_message"))
	defer srv.Close()
	s := fastSession(t, srv.URL, w.front)
	s.initialize()

	result := s.tool(2, "send_message", map[string]any{"to": "backend", "message": "only once please"})
	if result["isError"] == true {
		t.Fatalf("the retry should have hidden the drop: %s", toolText(result))
	}
	if n := w.messagesFrom(w.frontendID); n != 1 {
		t.Fatalf("stored %d copies", n)
	}
}

func TestLostInboxResponseDoesNotLoseMessages(t *testing.T) {
	w := newWorld(t)
	srv := httptest.NewServer(dropFirst(w.handler(), "check_inbox"))
	defer srv.Close()
	front := fastSession(t, srv.URL, w.front)
	front.initialize()
	front.tool(2, "send_message", map[string]any{"to": "backend", "message": "do not lose me"})

	back := fastSession(t, srv.URL, w.back)
	back.initialize()
	back.nextPush(5 * time.Second) // comes through the push path as well
	result := back.tool(3, "check_inbox", nil)
	if !strings.Contains(toolText(result), "do not lose me") {
		t.Fatalf("message lost with the response: %s", toolText(result))
	}
}

func TestProxyErrorsAreRetried(t *testing.T) {
	w := newWorld(t)
	var failures atomic.Int32
	failures.Store(3)
	real := w.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" && failures.Add(-1) >= 0 {
			http.Error(rw, "<html>502 Bad Gateway</html>", http.StatusBadGateway)
			return
		}
		real.ServeHTTP(rw, r)
	}))
	defer srv.Close()
	s := fastSession(t, srv.URL, w.front)
	s.initialize()
	if result := s.tool(2, "list_agents", nil); result["isError"] == true {
		t.Fatalf("502s should be retried: %s", toolText(result))
	}
}

func TestCutOffResponseIsRetried(t *testing.T) {
	w := newWorld(t)
	var cut atomic.Bool
	real := w.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" && cut.CompareAndSwap(false, true) {
			rw.Header().Set("Content-Type", "application/json")
			rw.Write([]byte(`{"jsonrpc":"2.0","id":2,"resu`))
			return
		}
		real.ServeHTTP(rw, r)
	}))
	defer srv.Close()
	s := fastSession(t, srv.URL, w.front)
	s.initialize()
	if result := s.tool(2, "list_agents", nil); result["isError"] == true {
		t.Fatalf("truncated JSON must not reach the session: %s", toolText(result))
	}
}

func TestGivesUpWithAClearMessageWhenOffline(t *testing.T) {
	s := fastSession(t, "http://127.0.0.1:1", "ac_x")
	s.initialize()
	result := s.tool(2, "send_message", map[string]any{"to": "backend", "message": "hi"})
	text := toolText(result)
	if result["isError"] != true || !strings.Contains(text, "get_history") {
		t.Fatalf("result: %s", text)
	}
}

// restartable is a hub that can be killed and started again on the same address.
// It tracks the raw sockets, because a test server's own cleanup leaves upgraded
// websockets running, which a real crash would not.
type restartable struct {
	w    *world
	addr string
	srv  *httptest.Server
	ln   *trackingListener
}

type trackingListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *trackingListener) cut() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		c.Close()
	}
}

func startRestartable(t *testing.T, w *world) *restartable {
	t.Helper()
	r := &restartable{w: w, addr: "127.0.0.1:0"}
	r.start()
	t.Cleanup(func() { r.stop() })
	return r
}

func (r *restartable) start() {
	r.w.t.Helper()
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ { // the old socket can take a moment to let go of the port
		ln, err = net.Listen("tcp", r.addr)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		r.w.t.Fatal(err)
	}
	r.addr = ln.Addr().String()
	r.ln = &trackingListener{Listener: ln}
	r.srv = httptest.NewUnstartedServer(r.w.handler())
	r.srv.Listener.Close()
	r.srv.Listener = r.ln
	r.srv.Start()
}

func (r *restartable) stop() {
	if r.srv != nil {
		r.ln.cut()
		r.srv.CloseClientConnections()
		r.srv.Close()
		r.srv = nil
	}
}

func (r *restartable) url() string {
	return "http://" + r.addr
}

func TestBridgeReconnectsAfterHubRestartAndGetsQueuedMessages(t *testing.T) {
	w := newWorld(t)
	hubProc := startRestartable(t, w)
	back := fastSession(t, hubProc.url(), w.back)
	back.initialize()
	time.Sleep(200 * time.Millisecond)

	hubProc.stop()
	time.Sleep(200 * time.Millisecond)
	hubProc.start()

	front := fastSession(t, hubProc.url(), w.front)
	front.initialize()
	front.tool(2, "send_message", map[string]any{"to": "backend", "message": "welcome back"})

	push := back.nextPush(10 * time.Second)
	if push["content"] != "welcome back" {
		t.Fatalf("push: %v", push)
	}
}

func TestAskSurvivesHubRestart(t *testing.T) {
	w := newWorld(t)
	hubProc := startRestartable(t, w)
	front := fastSession(t, hubProc.url(), w.front)
	back := fastSession(t, hubProc.url(), w.back)
	front.initialize()
	back.initialize()
	waitOnline(t, nil, front, "backend")

	answer := make(chan map[string]any, 1)
	go func() {
		answer <- front.tool(10, "ask_agent", map[string]any{"to": "backend", "message": "still there?", "wait_seconds": 20})
	}()
	push := back.nextPush(5 * time.Second)
	messageID, _ := strconv.ParseInt(push["meta"].(map[string]any)["message_id"].(string), 10, 64)

	// The hub dies while the asker is blocked in the tool call.
	hubProc.stop()
	time.Sleep(200 * time.Millisecond)
	hubProc.start()

	reply := back.tool(11, "reply", map[string]any{"message_id": messageID, "message": "yes, still here"})
	if reply["isError"] == true {
		t.Fatalf("reply: %s", toolText(reply))
	}
	select {
	case result := <-answer:
		if !strings.Contains(toolText(result), "yes, still here") {
			t.Fatalf("ask result: %s", toolText(result))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ask never finished after the restart")
	}
	if n := w.messagesFrom(w.frontendID); n != 1 {
		t.Fatalf("the question was stored %d times", n)
	}
}

// blackholeProxy forwards TCP until told to silently swallow everything on the
// connections it has so far: no close, no reset, like a dead network path.
type blackholeProxy struct {
	target string
	ln     net.Listener
	mu     sync.Mutex
	links  []*atomic.Bool
}

func newBlackholeProxy(t *testing.T, target string) *blackholeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &blackholeProxy{target: target, ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			upstream, err := net.Dial("tcp", target)
			if err != nil {
				client.Close()
				continue
			}
			dead := &atomic.Bool{}
			p.mu.Lock()
			p.links = append(p.links, dead)
			p.mu.Unlock()
			go pipe(client, upstream, dead)
			go pipe(upstream, client, dead)
		}
	}()
	return p
}

func pipe(from, to net.Conn, dead *atomic.Bool) {
	buf := make([]byte, 32*1024)
	for {
		n, err := from.Read(buf)
		if err != nil {
			if !dead.Load() {
				to.Close()
			}
			return
		}
		if dead.Load() {
			continue
		}
		if _, err := to.Write(buf[:n]); err != nil {
			return
		}
	}
}

func (p *blackholeProxy) swallowExisting() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, dead := range p.links {
		dead.Store(true)
	}
}

func TestDeadNetworkPathIsNoticedAndMessageStillArrives(t *testing.T) {
	w := newWorld(t)
	srv := httptest.NewServer(w.handler())
	defer srv.Close()
	proxy := newBlackholeProxy(t, strings.TrimPrefix(srv.URL, "http://"))

	back := fastSession(t, "http://"+proxy.ln.Addr().String(), w.back)
	back.initialize()
	time.Sleep(200 * time.Millisecond)

	// The path dies with no signal to either side, and a message is sent into the void.
	proxy.swallowExisting()
	front := fastSession(t, srv.URL, w.front)
	front.initialize()
	front.tool(2, "send_message", map[string]any{"to": "backend", "message": "are you getting this"})

	push := back.nextPush(10 * time.Second)
	if push["content"] != "are you getting this" {
		t.Fatalf("push: %v", push)
	}
}

func TestRevokedTokenStopsReconnecting(t *testing.T) {
	w := newWorld(t)
	srv := httptest.NewServer(w.handler())
	defer srv.Close()
	s := fastSession(t, srv.URL, "ac_never_valid")
	s.initialize()
	push := s.nextPush(5 * time.Second)
	if !strings.Contains(push["content"].(string), "rejected") {
		t.Fatalf("push: %v", push)
	}
	if push["meta"].(map[string]any)["kind"] != "system" {
		t.Fatalf("meta: %v", push["meta"])
	}
}

func TestWatchKeepsMessageUntilItHasItAndInboxStillFindsIt(t *testing.T) {
	w := newWorld(t)
	var dropped atomic.Bool
	real := w.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// The first inbox response dies on the way to the watcher.
		if strings.HasPrefix(r.URL.Path, "/api/agent/inbox") && r.Method == http.MethodGet && dropped.CompareAndSwap(false, true) {
			real.ServeHTTP(httptest.NewRecorder(), r)
			conn, _, err := rw.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		real.ServeHTTP(rw, r)
	}))
	defer srv.Close()

	front := fastSession(t, srv.URL, w.front)
	front.initialize()
	front.tool(2, "send_message", map[string]any{"to": "backend", "message": "survives the drop"})

	var stderr strings.Builder
	code := make(chan int, 1)
	go func() { code <- RunWatch(context.Background(), Config{URL: srv.URL, Token: w.back}, &stderr) }()
	select {
	case got := <-code:
		if got != exitWake || !strings.Contains(stderr.String(), "survives the drop") {
			t.Fatalf("exit %d, stderr %q", got, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watcher never delivered")
	}
	pending, _ := w.store.Undelivered(context.Background(), w.backendID, 10)
	if len(pending) != 0 {
		t.Fatalf("confirmed message still pending: %+v", pending)
	}
	// This is the bug a real session hit: the watcher must not empty the inbox, or a
	// session that calls check_inbox finds nothing and the question goes unanswered.
	back := fastSession(t, srv.URL, w.back)
	back.initialize()
	if text := toolText(back.tool(5, "check_inbox", nil)); !strings.Contains(text, "survives the drop") {
		t.Fatalf("inbox lost the message: %s", text)
	}
}

func TestPrintModeWatcherExitsAtOnce(t *testing.T) {
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "sdk-cli")
	done := make(chan int, 1)
	go func() {
		done <- RunWatch(context.Background(), Config{URL: "http://127.0.0.1:1", Token: "x"}, io.Discard)
	}()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a watcher in print mode keeps the whole process from exiting")
	}
}

func TestNoPushBridgeDeclaresNoChannelAndStaysQuiet(t *testing.T) {
	w := newWorld(t)
	srv := httptest.NewServer(w.handler())
	defer srv.Close()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	lines := make(chan map[string]any, 16)
	bridge := NewBridge(Config{URL: srv.URL, Token: w.back}, inR, outW, quietLog())
	bridge.NoPush = true
	bridge.pushDelay = 0
	go bridge.Run(context.Background())
	go readLines(t, outR, lines)
	defer inW.Close()

	io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n")
	io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	result := (<-lines)["result"].(map[string]any)
	if _, ok := result["capabilities"].(map[string]any)["experimental"]; ok {
		t.Fatal("declared a channel without a push connection")
	}
	if !strings.Contains(result["instructions"].(string), "agent_message") {
		t.Fatal("instructions describe the wrong delivery path")
	}

	front := fastSession(t, srv.URL, w.front)
	front.initialize()
	front.tool(2, "send_message", map[string]any{"to": "backend", "message": "for the watcher"})
	select {
	case m := <-lines:
		t.Fatalf("pushed anyway: %v", m)
	case <-time.After(300 * time.Millisecond):
	}
	// Still undelivered, so the watcher gets it.
	pending, _ := w.store.Undelivered(context.Background(), w.backendID, 10)
	if len(pending) != 1 {
		t.Fatalf("pending: %+v", pending)
	}
}

func TestReplacedSessionWaitsAndTakesOverAgain(t *testing.T) {
	w := newWorld(t)
	srv := httptest.NewServer(w.handler())
	defer srv.Close()

	older := fastSession(t, srv.URL, w.back)
	older.initialize()
	time.Sleep(200 * time.Millisecond)

	newer := fastSession(t, srv.URL, w.back)
	newer.initialize()

	notice := older.nextPush(5 * time.Second)
	if !strings.Contains(notice["content"].(string), "Another session") || notice["meta"].(map[string]any)["kind"] != "system" {
		t.Fatalf("notice: %v", notice)
	}

	// While the newer session lives it gets the messages, the older one stays quiet.
	front := fastSession(t, srv.URL, w.front)
	front.initialize()
	front.tool(2, "send_message", map[string]any{"to": "backend", "message": "for the newer one"})
	if got := newer.nextPush(5 * time.Second); got["content"] != "for the newer one" {
		t.Fatalf("newer got %v", got)
	}

	// The newer session ends, the older one takes the slot back.
	newer.stdin.Close()
	select {
	case <-newer.done:
	case <-time.After(5 * time.Second):
		t.Fatal("newer session did not end")
	}
	back := older.nextPush(5 * time.Second)
	if !strings.Contains(back["content"].(string), "again") {
		t.Fatalf("expected the all clear, got %v", back)
	}
	front.tool(3, "send_message", map[string]any{"to": "backend", "message": "for the older one"})
	// The earlier message may come along too: the newer session can end before its ack
	// reaches the hub, and delivery is at least once. What must not happen is a gap.
	for {
		got := older.nextPush(5 * time.Second)
		if got["content"] == "for the older one" {
			return
		}
		if got["content"] != "for the newer one" {
			t.Fatalf("older got %v", got)
		}
	}
}

func TestShortLivedProcessDoesNotDisturbRunningSession(t *testing.T) {
	w := newWorld(t)
	srv := httptest.NewServer(w.handler())
	defer srv.Close()

	running := fastSession(t, srv.URL, w.back)
	running.initialize()
	time.Sleep(200 * time.Millisecond)

	// A health check: start, handshake, list tools, gone within the delay.
	inR, inW := io.Pipe()
	bridge := NewBridge(Config{URL: srv.URL, Token: w.back}, inR, io.Discard, quietLog())
	bridge.pushDelay = 500 * time.Millisecond
	done := make(chan struct{})
	go func() {
		bridge.Run(context.Background())
		close(done)
	}()
	io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n")
	io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	inW.Close()
	<-done

	front := fastSession(t, srv.URL, w.front)
	front.initialize()
	front.tool(2, "send_message", map[string]any{"to": "backend", "message": "still here?"})
	got := running.nextPush(5 * time.Second)
	if got["content"] != "still here?" {
		t.Fatalf("running session was disturbed: %v", got)
	}
}

func TestAskerIsToldWhenTheOtherAgentIsRemoved(t *testing.T) {
	w := newWorld(t)
	h := w.handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	// The question is sent while backend is offline, so nobody is blocked on it.
	front := fastSession(t, srv.URL, w.front)
	front.initialize()
	time.Sleep(200 * time.Millisecond)
	var asked map[string]any
	json.Unmarshal([]byte(toolText(front.tool(2, "ask_agent", map[string]any{"to": "backend", "message": "are you there", "wait_seconds": 0}))), &asked)
	if asked["status"] != "queued" {
		t.Fatalf("asked: %v", asked)
	}

	// Removing it the way the web UI does: token gone, then the announcement.
	if err := w.removeBackendThroughAPI(srv.URL); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal([]byte(toolText(front.tool(3, "wait_for_reply", map[string]any{"message_id": asked["message_id"], "wait_seconds": 5}))), &got)
	if got["status"] != "unreachable" {
		t.Fatalf("got %v", got)
	}
	notice := front.nextPush(5 * time.Second)
	if notice["meta"].(map[string]any)["kind"] != "system" || !strings.Contains(notice["content"].(string), "backend was removed") {
		t.Fatalf("notice: %v", notice)
	}
}

func TestWatcherStopsOnRejectedTokenAndHookSaysSo(t *testing.T) {
	w := newWorld(t)
	srv := httptest.NewServer(w.handler())
	defer srv.Close()
	cfg := Config{URL: srv.URL, Token: "ac_rejected"}

	done := make(chan int, 1)
	go func() { done <- RunWatch(context.Background(), cfg, io.Discard) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("waking here would loop forever, exit %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watcher keeps retrying a token that will never work")
	}

	var stdout, stderr strings.Builder
	if code := RunHook(context.Background(), cfg, strings.NewReader(`{"hook_event_name":"UserPromptSubmit"}`), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout.String(), "rejected") || !strings.Contains(stdout.String(), "Tell them") {
		t.Fatalf("hook output: %q", stdout.String())
	}
}

// removeBackendThroughAPI signs in like the web UI and removes the agent.
func (w *world) removeBackendThroughAPI(base string) error {
	ctx := context.Background()
	hash, err := auth.HashPassword("a long enough password")
	if err != nil {
		return err
	}
	u, err := w.store.UserByUsername(ctx, "owner")
	if err != nil {
		return err
	}
	if err := w.store.SetPassword(ctx, u.ID, hash, ""); err != nil {
		return err
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.Post(base+"/api/auth/login", "application/json", strings.NewReader(`{"username":"owner","password":"a long enough password"}`))
	if err != nil {
		return err
	}
	resp.Body.Close()
	req, _ := http.NewRequest(http.MethodDelete, base+"/api/workspaces/1/agents/"+strconv.FormatInt(w.backendID, 10), nil)
	resp, err = c.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("remove agent: status %d", resp.StatusCode)
	}
	return nil
}
