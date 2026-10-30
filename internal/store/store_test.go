package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFirstAccountBecomesAdminOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, err := s.CreateAccount(ctx, "alice", "h", "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateAccount(ctx, "bob", "h", "", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if !a.IsAdmin || b.IsAdmin {
		t.Fatalf("admin flags wrong: alice=%v bob=%v", a.IsAdmin, b.IsAdmin)
	}
	if _, err := s.CreateAccount(ctx, "ALICE", "h", "", "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("case insensitive duplicate: got %v", err)
	}
}

func TestInviteLimits(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	owner, _ := s.CreateAccount(ctx, "owner", "h", "", "team")
	ws, _ := s.WorkspacesForUser(ctx, owner.ID)
	future := time.Now().Add(time.Hour).UnixMilli()
	if _, err := s.CreateInvite(ctx, ws[0].ID, owner.ID, "code", 1, future); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAccount(ctx, "first", "h", "code", ""); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, err := s.CreateAccount(ctx, "second", "h", "code", ""); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("second use: got %v", err)
	}
	// The failed registration must not leave an account behind.
	if _, err := s.UserByUsername(ctx, "second"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("account leaked after failed invite: %v", err)
	}
	if _, err := s.CreateInvite(ctx, ws[0].ID, owner.ID, "old", 5, time.Now().Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAccount(ctx, "third", "h", "old", ""); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("expired invite: got %v", err)
	}
}

func TestLastOwnerCannotLeave(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	owner, _ := s.CreateAccount(ctx, "owner", "h", "", "team")
	ws, _ := s.WorkspacesForUser(ctx, owner.ID)
	if err := s.RemoveMember(ctx, ws[0].ID, owner.ID); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("got %v", err)
	}
}

// fixture creates a workspace with two agents.
func fixture(t *testing.T, s *Store) (ws int64, a, b *Agent) {
	t.Helper()
	ctx := context.Background()
	u, err := s.CreateAccount(ctx, "owner", "h", "", "team")
	if err != nil {
		t.Fatal(err)
	}
	list, _ := s.WorkspacesForUser(ctx, u.ID)
	ws = list[0].ID
	a, err = s.CreateAgent(ctx, ws, u.ID, "frontend", "", "ta")
	if err != nil {
		t.Fatal(err)
	}
	b, err = s.CreateAgent(ctx, ws, u.ID, "backend", "", "tb")
	if err != nil {
		t.Fatal(err)
	}
	return ws, a, b
}

func TestThreadsAndInbox(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ws, a, b := fixture(t, s)

	ask, err := s.InsertMessage(ctx, ws, 0, a.ID, b.ID, "ask", "how does pagination work", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ask.ThreadID != ask.ID {
		t.Fatalf("new thread should take message id, got thread %d message %d", ask.ThreadID, ask.ID)
	}
	if ask.From != "frontend" || ask.To != "backend" {
		t.Fatalf("names not resolved: %+v", ask)
	}

	unread, _ := s.Unread(ctx, b.ID, 10)
	if len(unread) != 1 {
		t.Fatalf("want 1 unread, got %d", len(unread))
	}
	if err := s.MarkRead(ctx, b.ID, ask.ID); err != nil {
		t.Fatal(err)
	}
	unread, _ = s.Unread(ctx, b.ID, 10)
	if len(unread) != 0 {
		t.Fatalf("want 0 unread, got %d", len(unread))
	}

	reply, err := s.InsertMessage(ctx, ws, ask.ThreadID, b.ID, a.ID, "reply", "cursor based", ask.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ReplyTo(ctx, ask.ID)
	if err != nil || got.ID != reply.ID {
		t.Fatalf("reply lookup: %v %+v", err, got)
	}

	threads, err := s.Threads(ctx, ws, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].Messages != 2 || len(threads[0].Participants) != 2 {
		t.Fatalf("threads: %+v", threads)
	}
}

func TestMarkReadOnlyTouchesOwnMessages(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ws, a, b := fixture(t, s)
	m, _ := s.InsertMessage(ctx, ws, 0, a.ID, b.ID, "note", "hi", 0)
	if err := s.MarkRead(ctx, a.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	unread, _ := s.Unread(ctx, b.ID, 10)
	if len(unread) != 1 {
		t.Fatal("sender was able to mark the recipient's message as read")
	}
}

func TestSearchAndHistoryPaging(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ws, a, b := fixture(t, s)
	var last int64
	for _, body := range []string{"redis cache layer", "postgres schema", "the cache was stale"} {
		m, err := s.InsertMessage(ctx, ws, 0, a.ID, b.ID, "note", body, 0)
		if err != nil {
			t.Fatal(err)
		}
		last = m.ID
	}
	hits, err := s.Search(ctx, ws, "cache", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 hits, got %d", len(hits))
	}
	// FTS operators and stray quotes must be treated as plain text.
	if _, err := s.Search(ctx, ws, `cache" OR NOT (`, 10); err != nil {
		t.Fatalf("search must not fail on odd input: %v", err)
	}

	page, err := s.History(ctx, ws, HistoryFilter{AgentID: a.ID, WithID: b.ID, BeforeID: last, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].Body != "postgres schema" {
		t.Fatalf("paging: %+v", page)
	}
}

func TestPruneKeepsSearchIndexConsistent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ws, a, b := fixture(t, s)
	s.InsertMessage(ctx, ws, 0, a.ID, b.ID, "note", "ephemeral words", 0)
	n, err := s.PruneMessages(ctx, time.Now().Add(time.Hour).UnixMilli())
	if err != nil || n != 1 {
		t.Fatalf("prune: n=%d err=%v", n, err)
	}
	hits, err := s.Search(ctx, ws, "ephemeral", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("search after prune: %v %+v", err, hits)
	}
}

func TestWorkspaceIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ws, a, b := fixture(t, s)
	other, _ := s.CreateAccount(ctx, "other", "h", "", "elsewhere")
	otherWs, _ := s.WorkspacesForUser(ctx, other.ID)
	m, _ := s.InsertMessage(ctx, ws, 0, a.ID, b.ID, "note", "private words", 0)

	if _, err := s.MessageByID(ctx, otherWs[0].ID, m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("message visible across workspaces: %v", err)
	}
	hits, _ := s.Search(ctx, otherWs[0].ID, "private", 10)
	if len(hits) != 0 {
		t.Fatal("search crossed workspaces")
	}
	if _, err := s.AgentByName(ctx, otherWs[0].ID, "frontend"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("agent visible across workspaces: %v", err)
	}
}
