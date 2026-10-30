package hub

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orestislef/agentic-chat/internal/store"
)

type env struct {
	hub      *Hub
	store    *store.Store
	frontend *store.Agent
	backend  *store.Agent
}

func setup(t *testing.T, limits Limits) *env {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	u, err := s.CreateAccount(ctx, "owner", "h", "", "team")
	if err != nil {
		t.Fatal(err)
	}
	list, _ := s.WorkspacesForUser(ctx, u.ID)
	a, err := s.CreateAgent(ctx, list[0].ID, u.ID, "frontend", "", "ta")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateAgent(ctx, list[0].ID, u.ID, "backend", "", "tb")
	if err != nil {
		t.Fatal(err)
	}
	return &env{hub: New(s, limits, slog.New(slog.NewTextHandler(io.Discard, nil))), store: s, frontend: a, backend: b}
}

func TestAskAndReplyReachWaiterWithoutPush(t *testing.T) {
	e := setup(t, DefaultLimits())
	ctx := context.Background()

	frontendConn, err := e.hub.Attach(ctx, e.frontend)
	if err != nil {
		t.Fatal(err)
	}
	backendConn, err := e.hub.Attach(ctx, e.backend)
	if err != nil {
		t.Fatal(err)
	}

	ask, err := e.hub.Send(ctx, e.frontend, "backend", "ask", "which port", 0)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-backendConn.C:
		if m.ID != ask.ID {
			t.Fatalf("pushed wrong message: %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("backend did not get the push")
	}

	got := make(chan *store.Message, 1)
	go func() {
		m, err := e.hub.WaitReply(ctx, e.frontend, ask.ID, 5*time.Second)
		if err != nil {
			t.Error(err)
		}
		got <- m
	}()
	waitForWaiter(t, e.hub, ask.ID)

	if _, err := e.hub.Reply(ctx, e.backend, ask.ID, "8080"); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m == nil || m.Body != "8080" {
			t.Fatalf("waiter got %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter never woke")
	}
	select {
	case m := <-frontendConn.C:
		t.Fatalf("answer was pushed as well as returned: %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
	unread, _ := e.store.Unread(ctx, e.frontend.ID, 10)
	if len(unread) != 0 {
		t.Fatalf("answer handed to a waiter should count as read, got %d unread", len(unread))
	}
}

func waitForWaiter(t *testing.T, h *Hub, messageID int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		n := len(h.replyWaiters[messageID])
		h.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("waiter never registered")
}

func TestReplyAfterWaitTimeoutIsPushed(t *testing.T) {
	e := setup(t, DefaultLimits())
	ctx := context.Background()
	conn, _ := e.hub.Attach(ctx, e.frontend)
	ask, _ := e.hub.Send(ctx, e.frontend, "backend", "ask", "slow question", 0)

	m, err := e.hub.WaitReply(ctx, e.frontend, ask.ID, 10*time.Millisecond)
	if err != nil || m != nil {
		t.Fatalf("expected a clean timeout, got %+v %v", m, err)
	}
	if _, err := e.hub.Reply(ctx, e.backend, ask.ID, "late answer"); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-conn.C:
		if m.Body != "late answer" || m.ThreadID != ask.ThreadID {
			t.Fatalf("push: %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("late answer was not pushed")
	}
}

func TestOfflineMessagesArriveOnConnect(t *testing.T) {
	e := setup(t, DefaultLimits())
	ctx := context.Background()
	if _, err := e.hub.Send(ctx, e.frontend, "backend", "note", "while you were out", 0); err != nil {
		t.Fatal(err)
	}
	conn, err := e.hub.Attach(ctx, e.backend)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-conn.C:
		if m.Body != "while you were out" {
			t.Fatalf("got %+v", m)
		}
		if err := e.hub.Ack(ctx, e.backend.ID, m.ID); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued message not delivered")
	}
	e.hub.Detach(e.backend, conn)

	again, _ := e.hub.Attach(ctx, e.backend)
	select {
	case m := <-again.C:
		t.Fatalf("acked message delivered twice: %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestNewestConnectionWins(t *testing.T) {
	e := setup(t, DefaultLimits())
	ctx := context.Background()
	first, _ := e.hub.Attach(ctx, e.backend)
	second, _ := e.hub.Attach(ctx, e.backend)
	select {
	case <-first.Closed:
		if first.Reason != ReasonSuperseded {
			t.Fatalf("reason %q", first.Reason)
		}
	case <-time.After(time.Second):
		t.Fatal("old connection stayed open")
	}
	// The old socket closing later must not take the new connection offline.
	e.hub.Detach(e.backend, first)
	if e.hub.State(e.backend) != StateOnline {
		t.Fatal("new connection lost its presence")
	}
	e.hub.Send(ctx, e.frontend, "backend", "note", "hello", 0)
	select {
	case <-second.C:
	case <-time.After(time.Second):
		t.Fatal("new connection got nothing")
	}
}

func TestThreadLimitStopsLoops(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxThreadMessages = 3
	e := setup(t, limits)
	ctx := context.Background()
	first, err := e.hub.Send(ctx, e.frontend, "backend", "ask", "1", 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := e.hub.Send(ctx, e.backend, "frontend", "note", "more", first.ThreadID); err != nil {
			t.Fatal(err)
		}
	}
	_, err = e.hub.Send(ctx, e.backend, "frontend", "note", "one too many", first.ThreadID)
	if err == nil || !strings.Contains(err.Error(), "check with your user") {
		t.Fatalf("expected the loop guard, got %v", err)
	}
}

func TestRateLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.MessagesPerMinute = 2
	e := setup(t, limits)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := e.hub.Send(ctx, e.frontend, "backend", "note", "x", 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.hub.Send(ctx, e.frontend, "backend", "note", "x", 0); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("got %v", err)
	}
}

func TestValidation(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxMessageBytes = 5
	e := setup(t, limits)
	ctx := context.Background()
	cases := []struct {
		to, body string
		thread   int64
		want     string
	}{
		{"backend", "", 0, "empty"},
		{"backend", "too long body", 0, "limit"},
		{"frontend", "hi", 0, "itself"},
		{"nobody", "hi", 0, "no such agent"},
		{"backend", "hi", 999, "no such thread"},
	}
	for _, c := range cases {
		_, err := e.hub.Send(ctx, e.frontend, c.to, "note", c.body, c.thread)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("to=%s body=%q: got %v, want %q", c.to, c.body, err, c.want)
		}
	}
}

func TestOnlyRecipientCanReply(t *testing.T) {
	e := setup(t, DefaultLimits())
	ctx := context.Background()
	ask, _ := e.hub.Send(ctx, e.frontend, "backend", "ask", "q", 0)
	if _, err := e.hub.Reply(ctx, e.frontend, ask.ID, "answering myself"); !errors.Is(err, ErrNotRecipient) {
		t.Fatalf("got %v", err)
	}
	if _, err := e.hub.WaitReply(ctx, e.backend, ask.ID, time.Millisecond); !errors.Is(err, ErrNotSender) {
		t.Fatalf("got %v", err)
	}
}

func TestInboxWaitWakesOnMessage(t *testing.T) {
	e := setup(t, DefaultLimits())
	ctx := context.Background()
	got := make(chan []store.Message, 1)
	go func() {
		msgs, err := e.hub.Inbox(ctx, e.backend, InboxOptions{Limit: 10, Wait: 5 * time.Second})
		if err != nil {
			t.Error(err)
		}
		got <- msgs
	}()
	time.Sleep(30 * time.Millisecond)
	if e.hub.State(e.backend) != StateOnline {
		t.Fatal("a waiting inbox should count as online")
	}
	e.hub.Send(ctx, e.frontend, "backend", "note", "wake up", 0)
	select {
	case msgs := <-got:
		if len(msgs) != 1 || msgs[0].Body != "wake up" {
			t.Fatalf("got %+v", msgs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inbox wait did not wake")
	}
}

func TestExclusiveWatcherReplacesOlderOne(t *testing.T) {
	e := setup(t, DefaultLimits())
	ctx := context.Background()
	older := make(chan error, 1)
	go func() {
		_, err := e.hub.Inbox(ctx, e.backend, InboxOptions{Limit: 10, Wait: 5 * time.Second, Exclusive: true})
		older <- err
	}()
	time.Sleep(30 * time.Millisecond)
	newer := make(chan []store.Message, 1)
	go func() {
		msgs, _ := e.hub.Inbox(ctx, e.backend, InboxOptions{Limit: 10, Wait: 5 * time.Second, Exclusive: true})
		newer <- msgs
	}()
	select {
	case err := <-older:
		if !errors.Is(err, ErrWatcherReplaced) {
			t.Fatalf("older watcher should be told it was replaced, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("older watcher was not replaced")
	}
	time.Sleep(30 * time.Millisecond)
	e.hub.Send(ctx, e.frontend, "backend", "note", "for the newest", 0)
	select {
	case msgs := <-newer:
		if len(msgs) != 1 {
			t.Fatalf("got %+v", msgs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("newest watcher did not get the message")
	}
}

func TestRedeliverDrainsLargeBacklog(t *testing.T) {
	limits := DefaultLimits()
	limits.MessagesPerMinute = 1000
	e := setup(t, limits)
	ctx := context.Background()
	total := pushBuffer/2 + 20
	for i := 0; i < total; i++ {
		if _, err := e.hub.Send(ctx, e.frontend, "backend", "note", "x", 0); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := e.hub.Attach(ctx, e.backend)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	drain := func() {
		for {
			select {
			case m := <-conn.C:
				got[m.ID] = true
				e.hub.Ack(ctx, e.backend.ID, m.ID)
			default:
				return
			}
		}
	}
	drain()
	if len(got) >= total {
		t.Fatalf("test needs a backlog larger than one batch, got %d of %d", len(got), total)
	}
	if err := e.hub.Redeliver(ctx, e.backend, conn); err != nil {
		t.Fatal(err)
	}
	drain()
	if len(got) != total {
		t.Fatalf("delivered %d of %d", len(got), total)
	}
}

func TestRetriedSendIsStoredOnce(t *testing.T) {
	e := setup(t, DefaultLimits())
	ctx := WithIdempotencyKey(context.Background(), "key-1")
	conn, _ := e.hub.Attach(context.Background(), e.backend)
	first, err := e.hub.Send(ctx, e.frontend, "backend", "ask", "once", 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.hub.Send(ctx, e.frontend, "backend", "ask", "once", 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("retry created a new message: %d and %d", first.ID, second.ID)
	}
	<-conn.C
	select {
	case m := <-conn.C:
		t.Fatalf("retry was pushed again: %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPendingStaysUntilAckedAndUnreadAfterwards(t *testing.T) {
	e := setup(t, DefaultLimits())
	ctx := context.Background()
	m, _ := e.hub.Send(ctx, e.frontend, "backend", "note", "keep me", 0)
	for i := 0; i < 2; i++ {
		got, err := e.hub.Inbox(ctx, e.backend, InboxOptions{Limit: 10, Pending: true})
		if err != nil || len(got) != 1 {
			t.Fatalf("pending %d: %v %+v", i, err, got)
		}
	}
	if err := e.hub.Ack(ctx, e.backend.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := e.hub.Inbox(ctx, e.backend, InboxOptions{Limit: 10, Pending: true})
	if len(got) != 0 {
		t.Fatalf("still pending after the ack: %+v", got)
	}
	// Delivered is not read: the session may never have seen it, so the inbox keeps it.
	got, _ = e.hub.Inbox(ctx, e.backend, InboxOptions{Limit: 10})
	if len(got) != 1 || got[0].DeliveredAt == 0 {
		t.Fatalf("inbox lost a delivered message: %+v", got)
	}
}

func TestKickCutsEverythingOpen(t *testing.T) {
	e := setup(t, DefaultLimits())
	ctx := context.Background()
	conn, _ := e.hub.Attach(ctx, e.backend)
	waiter := make(chan error, 1)
	go func() {
		_, err := e.hub.Inbox(ctx, e.backend, InboxOptions{Limit: 10, Wait: 5 * time.Second})
		waiter <- err
	}()
	time.Sleep(30 * time.Millisecond)
	sub := e.hub.Subscribe(e.backend.WorkspaceID, e.backend.ID)

	e.hub.Kick(e.backend.WorkspaceID, e.backend.ID)

	select {
	case <-conn.Closed:
	case <-time.After(time.Second):
		t.Fatal("push connection stayed open")
	}
	select {
	case err := <-waiter:
		if !errors.Is(err, ErrWatcherReplaced) {
			t.Fatalf("waiter ended with %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("inbox wait stayed open")
	}
	select {
	case <-sub.Closed:
	case <-time.After(time.Second):
		t.Fatal("browser subscription stayed open")
	}
}
