// Package hub routes messages between agents and tracks who is online.
// All live state is in memory and rebuilds itself as clients reconnect, the
// store stays the source of truth for messages.
package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orestislef/agentic-chat/internal/store"
)

// Presence states reported for an agent.
const (
	StateOnline  = "online" // holds a live connection
	StateRecent  = "recent" // no live connection, but used its tools a moment ago
	StateOffline = "offline"
)

// recentWindow is how long an agent without a live connection still counts as reachable.
const recentWindow = 2 * time.Minute

// pushBuffer is how many messages can wait for one slow connection before it gets dropped.
// A dropped client reconnects and picks up whatever is still undelivered.
const pushBuffer = 256

// Errors shown to the agent, so they say what to do next.
var (
	ErrEmptyBody     = errors.New("message is empty")
	ErrSelfMessage   = errors.New("an agent cannot message itself")
	ErrUnknownAgent  = errors.New("no such agent in this workspace, use list_agents to see who is available")
	ErrUnknownThread = errors.New("no such thread in this workspace")
	ErrNotRecipient  = errors.New("you can only reply to messages addressed to you")
	ErrNotSender     = errors.New("you can only wait for replies to messages you sent")
	ErrRateLimited   = errors.New("too many messages, wait a minute before sending more")
	ErrLimitReached  = errors.New("limit reached")

	// ErrRecipientGone means a question can no longer be answered.
	ErrRecipientGone = errors.New("that agent was removed from the workspace, this question will not be answered")

	// ErrWatcherReplaced tells a background watcher that a newer one took over and it should exit.
	ErrWatcherReplaced = errors.New("replaced by a newer watcher")
)

// Limits protects the hub and keeps two agents from talking in circles forever.
type Limits struct {
	MaxThreadMessages int
	MaxMessageBytes   int
	MessagesPerMinute int
}

// DefaultLimits are used unless the operator overrides them.
func DefaultLimits() Limits {
	return Limits{MaxThreadMessages: 50, MaxMessageBytes: 64 * 1024, MessagesPerMinute: 120}
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidName reports whether a string works as an agent or user name. The set is
// narrow on purpose: names end up in tag attributes inside the agent's context.
func ValidName(name string) bool {
	return nameRE.MatchString(name)
}

// Event is what the web UI receives over its websocket.
type Event struct {
	Type    string         `json:"type"` // "message", "presence" or "agents"
	Message *store.Message `json:"message,omitempty"`
	AgentID int64          `json:"agent_id,omitempty"`
	State   string         `json:"state,omitempty"`
}

// Conn is the live push connection of an agent. Only the newest connection per
// agent receives messages, an older one is told why it was replaced.
type Conn struct {
	AgentID int64
	C       chan store.Message
	// Closed is closed when the hub ends this connection, Reason says why.
	Closed chan struct{}
	Reason string
}

// Close reasons.
const (
	ReasonSuperseded = "superseded"
	ReasonRevoked    = "revoked"
	ReasonOverflow   = "overflow"
)

// Subscriber is a web UI connection watching one workspace.
type Subscriber struct {
	WorkspaceID int64
	HumanID     int64
	C           chan Event
	Closed      chan struct{}
}

type inboxWaiter struct {
	wake      chan struct{}
	cancel    chan struct{}
	exclusive bool
}

type rateWindow struct {
	start time.Time
	count int
}

// Hub is safe for concurrent use.
type Hub struct {
	store  *store.Store
	limits Limits
	log    *slog.Logger

	mu           sync.Mutex
	conns        map[int64]*Conn
	subs         map[int64]map[*Subscriber]struct{}
	humans       map[int64]int
	replyWaiters map[int64]map[chan *store.Message]struct{}
	waitTargets  map[int64]int64 // who a waited for message is addressed to
	inboxWaiters map[int64]map[*inboxWaiter]struct{}
	rates        map[int64]*rateWindow
}

// New creates a hub.
func New(s *store.Store, limits Limits, log *slog.Logger) *Hub {
	return &Hub{
		store:        s,
		limits:       limits,
		log:          log,
		conns:        map[int64]*Conn{},
		subs:         map[int64]map[*Subscriber]struct{}{},
		humans:       map[int64]int{},
		replyWaiters: map[int64]map[chan *store.Message]struct{}{},
		waitTargets:  map[int64]int64{},
		inboxWaiters: map[int64]map[*inboxWaiter]struct{}{},
		rates:        map[int64]*rateWindow{},
	}
}

// State returns the presence of an agent.
func (h *Hub) State(a *store.Agent) string {
	h.mu.Lock()
	live := h.conns[a.ID] != nil || h.humans[a.ID] > 0 || len(h.inboxWaiters[a.ID]) > 0
	h.mu.Unlock()
	if live {
		return StateOnline
	}
	if a.LastSeenAt != 0 && time.Since(time.UnixMilli(a.LastSeenAt)) < recentWindow {
		return StateRecent
	}
	return StateOffline
}

// Send validates and stores a message, then hands it to whoever is listening.
func (h *Hub) Send(ctx context.Context, from *store.Agent, toName, kind, body string, threadID int64) (*store.Message, error) {
	to, err := h.store.AgentByName(ctx, from.WorkspaceID, toName)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrUnknownAgent
	}
	if err != nil {
		return nil, err
	}
	return h.send(ctx, from, to, kind, body, threadID, 0)
}

// Reply answers a message. The answer goes to the original sender in the same thread.
func (h *Hub) Reply(ctx context.Context, from *store.Agent, messageID int64, body string) (*store.Message, error) {
	original, err := h.store.MessageByID(ctx, from.WorkspaceID, messageID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("message %d not found", messageID)
	}
	if err != nil {
		return nil, err
	}
	if original.ToID != from.ID {
		return nil, ErrNotRecipient
	}
	to, err := h.store.AgentByID(ctx, from.WorkspaceID, original.FromID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%s no longer exists, the reply cannot be delivered", original.From)
	}
	if err != nil {
		return nil, err
	}
	m, err := h.send(ctx, from, to, "reply", body, original.ThreadID, original.ID)
	if err != nil {
		return nil, err
	}
	if err := h.store.MarkRead(ctx, from.ID, original.ID); err != nil {
		h.log.Warn("mark read after reply", "err", err)
	}
	return m, nil
}

func (h *Hub) send(ctx context.Context, from, to *store.Agent, kind, body string, threadID, inReplyTo int64) (*store.Message, error) {
	// A retry of something already stored skips the checks. It already passed them,
	// and the thread or rate limit may have filled up in the meantime.
	if key := idempotencyKey(ctx); key != "" {
		m, err := h.store.MessageByClientKey(ctx, from.ID, key)
		if err == nil {
			return m, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	if body == "" {
		return nil, ErrEmptyBody
	}
	if len(body) > h.limits.MaxMessageBytes {
		return nil, fmt.Errorf("%w: message is %d bytes, the limit is %d, send a shorter summary", ErrLimitReached, len(body), h.limits.MaxMessageBytes)
	}
	if from.ID == to.ID {
		return nil, ErrSelfMessage
	}
	if threadID != 0 {
		n, err := h.store.ThreadLength(ctx, from.WorkspaceID, threadID)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, ErrUnknownThread
		}
		if n >= h.limits.MaxThreadMessages {
			return nil, fmt.Errorf("%w: thread %d has %d messages, stop here and check with your user before continuing", ErrLimitReached, threadID, h.limits.MaxThreadMessages)
		}
	}
	if !h.allow(from.ID) {
		return nil, ErrRateLimited
	}
	m, created, err := h.store.InsertMessageKeyed(ctx, from.WorkspaceID, threadID, from.ID, to.ID, kind, body, inReplyTo, idempotencyKey(ctx))
	if err != nil {
		return nil, err
	}
	if created {
		h.dispatch(ctx, m)
	}
	return m, nil
}

type idempotencyKeyType struct{}

// WithIdempotencyKey marks the sends in ctx as retries of one logical send, so a
// client that never saw the answer can safely try again.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, idempotencyKeyType{}, key)
}

func idempotencyKey(ctx context.Context) string {
	key, _ := ctx.Value(idempotencyKeyType{}).(string)
	return key
}

func (h *Hub) allow(agentID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.rates[agentID]
	now := time.Now()
	if w == nil || now.Sub(w.start) >= time.Minute {
		h.rates[agentID] = &rateWindow{start: now, count: 1}
		return true
	}
	if w.count >= h.limits.MessagesPerMinute {
		return false
	}
	w.count++
	return true
}

// dispatch delivers a stored message. A reply somebody is actively waiting for
// goes straight to that waiter and is not pushed as well, otherwise the asker
// would see the same answer twice: once as the tool result and once as a push.
func (h *Hub) dispatch(ctx context.Context, m *store.Message) {
	event := Event{Type: "message", Message: m}
	h.mu.Lock()
	answered := false
	if m.InReplyTo != 0 {
		for ch := range h.replyWaiters[m.InReplyTo] {
			select {
			case ch <- m:
				answered = true
			default:
			}
		}
	}
	if !answered {
		for w := range h.inboxWaiters[m.ToID] {
			select {
			case w.wake <- struct{}{}:
			default:
			}
		}
		if c := h.conns[m.ToID]; c != nil {
			h.pushLocked(c, *m)
		}
	}
	for sub := range h.subs[m.WorkspaceID] {
		h.publishLocked(sub, event)
	}
	h.mu.Unlock()
	if answered {
		if err := h.store.MarkRead(ctx, m.ToID, m.ID); err != nil {
			h.log.Warn("mark read for waiter", "err", err)
		}
	}
}

func (h *Hub) pushLocked(c *Conn, m store.Message) {
	select {
	case c.C <- m:
	default:
		h.closeConnLocked(c, ReasonOverflow)
	}
}

func (h *Hub) closeConnLocked(c *Conn, reason string) {
	if h.conns[c.AgentID] == c {
		delete(h.conns, c.AgentID)
	}
	select {
	case <-c.Closed:
	default:
		c.Reason = reason
		close(c.Closed)
	}
}

func (h *Hub) publishLocked(sub *Subscriber, e Event) {
	select {
	case sub.C <- e:
	default:
		// Too slow to keep up. The UI reconnects and reloads from the API.
		select {
		case <-sub.Closed:
		default:
			close(sub.Closed)
		}
	}
}

// Attach registers the live connection of an agent and queues what it missed.
// A previous connection for the same agent is closed: newest wins, because the
// usual cause is a restarted session whose old socket has not timed out yet.
func (h *Hub) Attach(ctx context.Context, a *store.Agent) (*Conn, error) {
	c := &Conn{AgentID: a.ID, C: make(chan store.Message, pushBuffer), Closed: make(chan struct{})}
	h.mu.Lock()
	if old := h.conns[a.ID]; old != nil {
		h.closeConnLocked(old, ReasonSuperseded)
	}
	h.conns[a.ID] = c
	h.publishPresenceLocked(a.WorkspaceID, a.ID, StateOnline)
	h.mu.Unlock()

	// Anything dispatched between here and the query may appear twice. Clients
	// drop duplicates by message id, which is simpler than a lock around the query.
	pending, err := h.store.Undelivered(ctx, a.ID, pushBuffer/2)
	if err != nil {
		h.Detach(a, c)
		return nil, err
	}
	h.mu.Lock()
	for _, m := range pending {
		h.pushLocked(c, m)
	}
	h.mu.Unlock()
	return c, nil
}

// Redeliver pushes what is still unacknowledged on a live connection. It covers a
// backlog larger than Attach sends at once and an ack lost on the way. Clients drop
// messages they already have, so pushing one twice is harmless.
func (h *Hub) Redeliver(ctx context.Context, a *store.Agent, c *Conn) error {
	pending, err := h.store.Undelivered(ctx, a.ID, pushBuffer/2)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[a.ID] != c {
		return nil
	}
	for _, m := range pending {
		h.pushLocked(c, m)
	}
	return nil
}

// AgentGone tells whoever is affected that an agent was removed. A caller blocked on
// one of its answers wakes up right away. Others who asked it something and are still
// waiting get a notice, so nobody keeps counting on an answer that will never come.
func (h *Hub) AgentGone(ctx context.Context, a *store.Agent) {
	asks, err := h.store.UnansweredAsks(ctx, a.ID, 200)
	if err != nil {
		h.log.Warn("list unanswered asks", "agent", a.Name, "err", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for messageID, target := range h.waitTargets {
		if target != a.ID {
			continue
		}
		for ch := range h.replyWaiters[messageID] {
			select {
			case ch <- nil:
			default:
			}
		}
	}
	perAsker := map[int64][]string{}
	for _, m := range asks {
		if len(h.replyWaiters[m.ID]) == 0 { // those learn it from their tool result
			perAsker[m.FromID] = append(perAsker[m.FromID], strconv.FormatInt(m.ID, 10))
		}
	}
	for asker, ids := range perAsker {
		if c := h.conns[asker]; c != nil {
			h.pushLocked(c, store.Message{Kind: "system", Body: fmt.Sprintf(
				"%s was removed from the workspace, so your question (message %s) will not be answered. Tell your user if it matters.",
				a.Name, strings.Join(ids, ", "))})
		}
	}
	for sub := range h.subs[a.WorkspaceID] {
		h.publishLocked(sub, Event{Type: "agents"})
	}
}

// AgentsChanged tells open web UIs to reload the list of agents.
func (h *Hub) AgentsChanged(workspaceID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs[workspaceID] {
		h.publishLocked(sub, Event{Type: "agents"})
	}
}

// Connected reports whether an agent holds a live push connection right now.
func (h *Hub) Connected(agentID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[agentID] != nil
}

// Detach removes a connection when its socket ends.
func (h *Hub) Detach(a *store.Agent, c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[a.ID] != c {
		return
	}
	delete(h.conns, a.ID)
	if len(h.inboxWaiters[a.ID]) == 0 {
		h.publishPresenceLocked(a.WorkspaceID, a.ID, StateOffline)
	}
}

// Kick cuts everything a participant has open: the push connection, waiting inbox
// calls and, for a person, browser tabs still watching the workspace. Access has to
// end when it is taken away, not whenever the socket happens to close.
func (h *Hub) Kick(workspaceID, agentID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.conns[agentID]; c != nil {
		h.closeConnLocked(c, ReasonRevoked)
	}
	for w := range h.inboxWaiters[agentID] {
		close(w.cancel)
		delete(h.inboxWaiters[agentID], w)
	}
	for sub := range h.subs[workspaceID] {
		if sub.HumanID != agentID {
			continue
		}
		select {
		case <-sub.Closed:
		default:
			close(sub.Closed)
		}
	}
	h.publishPresenceLocked(workspaceID, agentID, StateOffline)
}

// Ack records that messages reached the client. They stay unread, because reaching
// the client does not prove the session saw them: Claude Code drops pushes silently
// when a session was not started with channels enabled. check_inbox still lists them.
func (h *Hub) Ack(ctx context.Context, agentID int64, messageIDs ...int64) error {
	return h.store.MarkDelivered(ctx, agentID, messageIDs...)
}

// Subscribe registers a web UI connection for a workspace.
func (h *Hub) Subscribe(workspaceID, humanID int64) *Subscriber {
	sub := &Subscriber{WorkspaceID: workspaceID, HumanID: humanID, C: make(chan Event, pushBuffer), Closed: make(chan struct{})}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[workspaceID] == nil {
		h.subs[workspaceID] = map[*Subscriber]struct{}{}
	}
	h.subs[workspaceID][sub] = struct{}{}
	h.humans[humanID]++
	if h.humans[humanID] == 1 {
		h.publishPresenceLocked(workspaceID, humanID, StateOnline)
	}
	return sub
}

// Unsubscribe removes a web UI connection.
func (h *Hub) Unsubscribe(sub *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs[sub.WorkspaceID], sub)
	if len(h.subs[sub.WorkspaceID]) == 0 {
		delete(h.subs, sub.WorkspaceID)
	}
	h.humans[sub.HumanID]--
	if h.humans[sub.HumanID] <= 0 {
		delete(h.humans, sub.HumanID)
		h.publishPresenceLocked(sub.WorkspaceID, sub.HumanID, StateOffline)
	}
}

func (h *Hub) publishPresenceLocked(workspaceID, agentID int64, state string) {
	for sub := range h.subs[workspaceID] {
		h.publishLocked(sub, Event{Type: "presence", AgentID: agentID, State: state})
	}
}

// WaitReply blocks until a reply to messageID arrives or the wait ends, in which
// case it returns nil and no error.
func (h *Hub) WaitReply(ctx context.Context, a *store.Agent, messageID int64, wait time.Duration) (*store.Message, error) {
	original, err := h.store.MessageByID(ctx, a.WorkspaceID, messageID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("message %d not found", messageID)
	}
	if err != nil {
		return nil, err
	}
	if original.FromID != a.ID {
		return nil, ErrNotSender
	}

	ch := make(chan *store.Message, 1)
	h.mu.Lock()
	if h.replyWaiters[messageID] == nil {
		h.replyWaiters[messageID] = map[chan *store.Message]struct{}{}
	}
	h.replyWaiters[messageID][ch] = struct{}{}
	h.waitTargets[messageID] = original.ToID
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.replyWaiters[messageID], ch)
		if len(h.replyWaiters[messageID]) == 0 {
			delete(h.replyWaiters, messageID)
			delete(h.waitTargets, messageID)
		}
		h.mu.Unlock()
	}()

	// The reply may have landed before the waiter was registered.
	existing, err := h.store.ReplyTo(ctx, messageID)
	if err == nil {
		if err := h.store.MarkRead(ctx, a.ID, existing.ID); err != nil {
			return nil, err
		}
		return existing, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if _, err := h.store.AgentByID(ctx, a.WorkspaceID, original.ToID); errors.Is(err, store.ErrNotFound) {
		return nil, ErrRecipientGone
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case m := <-ch:
		if m == nil {
			return nil, ErrRecipientGone
		}
		return m, nil
	case <-timer.C:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// InboxOptions controls an inbox query.
type InboxOptions struct {
	Limit int
	Wait  time.Duration
	// Exclusive replaces any earlier exclusive wait of the same agent, which keeps
	// stale background watchers from swallowing messages.
	Exclusive bool
	// Pending returns what has not been handed to the session yet and changes nothing.
	// It is the push path for clients that poll: they confirm with Ack once they have
	// the messages, so a response lost on the way loses nothing.
	Pending bool
}

// Inbox returns unread messages and marks them read, or with Pending what is still
// undelivered. With a wait it blocks until at least one arrives.
func (h *Hub) Inbox(ctx context.Context, a *store.Agent, opts InboxOptions) ([]store.Message, error) {
	limit, wait, exclusive := opts.Limit, opts.Wait, opts.Exclusive
	w := &inboxWaiter{wake: make(chan struct{}, 1), cancel: make(chan struct{}), exclusive: exclusive}
	if wait > 0 {
		h.mu.Lock()
		if h.inboxWaiters[a.ID] == nil {
			h.inboxWaiters[a.ID] = map[*inboxWaiter]struct{}{}
		}
		if exclusive {
			for other := range h.inboxWaiters[a.ID] {
				if other.exclusive {
					close(other.cancel)
					delete(h.inboxWaiters[a.ID], other)
				}
			}
		}
		h.inboxWaiters[a.ID][w] = struct{}{}
		h.publishPresenceLocked(a.WorkspaceID, a.ID, StateOnline)
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			delete(h.inboxWaiters[a.ID], w)
			empty := len(h.inboxWaiters[a.ID]) == 0
			if empty {
				delete(h.inboxWaiters, a.ID)
			}
			if empty && h.conns[a.ID] == nil {
				h.publishPresenceLocked(a.WorkspaceID, a.ID, StateOffline)
			}
			h.mu.Unlock()
		}()
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		var messages []store.Message
		var err error
		if opts.Pending {
			messages, err = h.store.Undelivered(ctx, a.ID, limit)
		} else {
			messages, err = h.store.Unread(ctx, a.ID, limit)
		}
		if err != nil {
			return nil, err
		}
		if len(messages) > 0 && opts.Pending {
			return messages, nil
		}
		if len(messages) > 0 {
			ids := make([]int64, len(messages))
			for i, m := range messages {
				ids[i] = m.ID
			}
			if err := h.store.MarkRead(ctx, a.ID, ids...); err != nil {
				return nil, err
			}
			return messages, nil
		}
		if wait <= 0 {
			return messages, nil
		}
		select {
		case <-w.wake:
		case <-w.cancel:
			return nil, ErrWatcherReplaced
		case <-timer.C:
			return []store.Message{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
