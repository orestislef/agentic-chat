package store

import (
	"context"
	"errors"
	"os"
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

	threads, err := s.Threads(ctx, ws, 10)
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

func TestDamagedDatabaseRefusesToOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range data {
		data[i] = 0xAB
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(path + "-wal")
	os.Remove(path + "-shm")
	if _, err := Open(path); err == nil {
		t.Fatal("opened a damaged database without complaint")
	}
}

func TestDatabaseWithBadPagesIsDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bitrot.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ws, a, b := fixture(t, s)
	for i := 0; i < 500; i++ {
		if _, err := s.InsertMessage(context.Background(), ws, 0, a.ID, b.ID, "note", "some text to fill the pages", 0); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the header intact and scribble over everything after the first pages.
	for i := 8192; i < len(data); i++ {
		data[i] = 0xAB
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("did not notice damaged pages")
	}
}

func TestBackupIsUsableAndLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	ws, a, b := fixture(t, s)
	s.InsertMessage(ctx, ws, 0, a.ID, b.ID, "note", "worth keeping", 0)

	target := filepath.Join(dir, "copy.db")
	if err := s.Backup(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
	restored, err := Open(target)
	if err != nil {
		t.Fatalf("backup does not open: %v", err)
	}
	defer restored.Close()
	hits, err := restored.Search(ctx, ws, "keeping", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("backup content: %v %+v", err, hits)
	}
}

func TestRetryKeyStoresOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ws, a, b := fixture(t, s)
	first, created, err := s.InsertMessageKeyed(ctx, ws, 0, a.ID, b.ID, "ask", "q", 0, "k1")
	if err != nil || !created {
		t.Fatalf("first: %v created=%v", err, created)
	}
	second, created, err := s.InsertMessageKeyed(ctx, ws, 0, a.ID, b.ID, "ask", "q", 0, "k1")
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("retry: %v created=%v id=%d", err, created, second.ID)
	}
	// The key belongs to the sender, another agent using the same value is unrelated.
	_, created, err = s.InsertMessageKeyed(ctx, ws, 0, b.ID, a.ID, "ask", "q", 0, "k1")
	if err != nil || !created {
		t.Fatalf("other sender: %v created=%v", err, created)
	}
}

func TestPragmasApply(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	expect := map[string]string{"journal_mode": "wal", "auto_vacuum": "2", "synchronous": "2", "foreign_keys": "1"}
	for pragma, want := range expect {
		var got string
		if err := s.w.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %s, want %s", pragma, got, want)
		}
	}
}

func TestMaintainCleansUp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ws, a, b := fixture(t, s)
	owner := a.UserID

	// An expired invite, a used up invite and one that is still good.
	s.CreateInvite(ctx, ws, owner, "expired", 5, time.Now().Add(-time.Hour).UnixMilli())
	s.CreateInvite(ctx, ws, owner, "used", 1, time.Now().Add(time.Hour).UnixMilli())
	s.CreateAccount(ctx, "guest", "h", "used", "")
	s.CreateInvite(ctx, ws, owner, "good", 5, time.Now().Add(time.Hour).UnixMilli())

	// A revoked agent without history goes, one that history names stays.
	gone, _ := s.CreateAgent(ctx, ws, owner, "gone", "", "tg")
	s.RevokeAgent(ctx, ws, gone.ID)
	s.InsertMessage(ctx, ws, 0, a.ID, b.ID, "note", "history", 0)
	s.RevokeAgent(ctx, ws, a.ID)

	s.CreateSession(ctx, "old", owner, time.Now().Add(-time.Hour).UnixMilli())

	if err := s.Maintain(ctx); err != nil {
		t.Fatal(err)
	}

	var invites, agents, sessions int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM invites`).Scan(&invites)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE id = ?`, gone.ID).Scan(&agents)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&sessions)
	if invites != 1 || agents != 0 || sessions != 0 {
		t.Fatalf("invites=%d revoked agent rows=%d sessions=%d", invites, agents, sessions)
	}
	hist, err := s.History(ctx, ws, HistoryFilter{Limit: 10})
	if err != nil || len(hist) != 1 || hist[0].From != "frontend" {
		t.Fatalf("history must keep naming a revoked agent: %v %+v", err, hist)
	}
}

func TestPruneInBatchesAndReclaimsSpace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ws, a, b := fixture(t, s)
	big := make([]byte, 4096)
	for i := range big {
		big[i] = 'x'
	}
	for i := 0; i < 2500; i++ {
		if _, err := s.InsertMessage(ctx, ws, 0, a.ID, b.ID, "note", string(big), 0); err != nil {
			t.Fatal(err)
		}
	}
	pages := func() (n int) {
		s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&n)
		return n
	}
	before := pages()
	n, err := s.PruneMessages(ctx, time.Now().Add(time.Hour).UnixMilli())
	if err != nil || n != 2500 {
		t.Fatalf("pruned %d: %v", n, err)
	}
	for i := 0; i < 5; i++ { // one run frees a limited number of pages
		if err := s.Maintain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if after := pages(); after >= before/2 {
		t.Fatalf("disk space not given back: %d pages before, %d after", before, after)
	}
}

func TestDemoPruneOnlyTouchesDemoRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Everything from before the flag: the admin, a colleague and their workspaces.
	admin, _ := s.CreateAccount(ctx, "admin", "h", "", "admin space")
	before, _ := s.CreateAccount(ctx, "colleague", "h", "", "colleague space")
	beforeWs, _ := s.WorkspacesForUser(ctx, before.ID)
	a, _ := s.CreateAgent(ctx, beforeWs[0].ID, before.ID, "keep", "", "t1")
	b, _ := s.CreateAgent(ctx, beforeWs[0].ID, before.ID, "keep2", "", "t2")
	s.InsertMessage(ctx, beforeWs[0].ID, 0, a.ID, b.ID, "note", "real words", 0)

	s.SetDemo(true)
	visitor, err := s.CreateAccount(ctx, "visitor", "h", "", "visitor space")
	if err != nil {
		t.Fatal(err)
	}
	vw, _ := s.WorkspacesForUser(ctx, visitor.ID)
	if !vw[0].Demo {
		t.Fatal("workspace created under demo mode is not marked")
	}
	x, _ := s.CreateAgent(ctx, vw[0].ID, visitor.ID, "x", "", "t3")
	y, _ := s.CreateAgent(ctx, vw[0].ID, visitor.ID, "y", "", "t4")
	s.InsertMessage(ctx, vw[0].ID, 0, x.ID, y.ID, "note", "throwaway words", 0)

	// Admins stay exempt, also for what they create while the flag is on.
	adminExtra, err := s.CreateWorkspace(ctx, "second", admin)
	if err != nil || adminExtra.Demo {
		t.Fatalf("admin workspace marked as demo: %v %+v", err, adminExtra)
	}

	wiped, users, err := s.PruneDemo(ctx, time.Now().Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if wiped != 1 || users != 1 {
		t.Fatalf("wiped %d workspaces and %d users", wiped, users)
	}
	if _, err := s.UserByUsername(ctx, "visitor"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("visitor still there: %v", err)
	}
	for _, name := range []string{"admin", "colleague"} {
		if _, err := s.UserByUsername(ctx, name); err != nil {
			t.Fatalf("%s was deleted: %v", name, err)
		}
	}
	if hits, _ := s.Search(ctx, beforeWs[0].ID, "real", 10); len(hits) != 1 {
		t.Fatalf("existing history was touched: %+v", hits)
	}
	// The search index must not keep what was deleted.
	var left int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'throwaway'`).Scan(&left)
	if left != 0 {
		t.Fatal("search index still holds deleted demo messages")
	}
	if err := s.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestThreadsNewestFirstAcrossPages(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ws, a, b := fixture(t, s)
	first, _ := s.InsertMessage(ctx, ws, 0, a.ID, b.ID, "ask", "the oldest thread", 0)
	// Enough newer messages in one thread to push the first thread past a whole page.
	second, _ := s.InsertMessage(ctx, ws, 0, a.ID, b.ID, "ask", "the busy thread", 0)
	for i := 0; i < 600; i++ {
		if _, err := s.InsertMessage(ctx, ws, second.ThreadID, b.ID, a.ID, "note", "more", 0); err != nil {
			t.Fatal(err)
		}
	}
	threads, err := s.Threads(ctx, ws, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 2 || threads[0].ID != second.ThreadID || threads[1].ID != first.ThreadID {
		t.Fatalf("threads: %+v", threads)
	}
	if threads[0].Messages != 601 || threads[0].Preview != "the busy thread" || len(threads[0].Participants) != 2 {
		t.Fatalf("summary: %+v", threads[0])
	}
	// Moving an old thread back to the top.
	s.InsertMessage(ctx, ws, first.ThreadID, b.ID, a.ID, "reply", "back again", first.ID)
	threads, _ = s.Threads(ctx, ws, 1)
	if len(threads) != 1 || threads[0].ID != first.ThreadID {
		t.Fatalf("limit and order: %+v", threads)
	}
	empty, _ := s.Threads(ctx, ws+999, 10)
	if len(empty) != 0 {
		t.Fatalf("other workspace: %+v", empty)
	}
}

func TestSearchFindsBestAmongRecentAndStaysInWorkspace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ws, a, b := fixture(t, s)
	s.InsertMessage(ctx, ws, 0, a.ID, b.ID, "note", "cache", 0)
	s.InsertMessage(ctx, ws, 0, a.ID, b.ID, "note", "a long message that mentions cache once among a lot of other unrelated words here", 0)
	hits, err := s.Search(ctx, ws, "cache", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Body != "cache" {
		t.Fatalf("best match should come first: %+v", hits)
	}
}

func TestConcurrentWritesAllLandAndFailuresStayAlone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ws, a, b := fixture(t, s)
	const n = 400
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			// Every tenth one breaks a foreign key, which must not take its batch down with it.
			to := b.ID
			if i%10 == 0 {
				to = 999999
			}
			_, err := s.InsertMessage(ctx, ws, 0, a.ID, to, "note", "x", 0)
			if i%10 == 0 && err == nil {
				err = errors.New("broken insert was accepted")
			} else if i%10 == 0 {
				err = nil
			}
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var stored int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&stored)
	if stored != n-n/10 {
		t.Fatalf("stored %d of %d", stored, n-n/10)
	}
}

func TestCancelledWriterDoesNotBreakOthers(t *testing.T) {
	s := newTestStore(t)
	ws, a, b := fixture(t, s)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	s.InsertMessage(cancelled, ws, 0, a.ID, b.ID, "note", "maybe", 0)
	if _, err := s.InsertMessage(context.Background(), ws, 0, a.ID, b.ID, "note", "for sure", 0); err != nil {
		t.Fatal(err)
	}
}

func TestWriteAfterCloseFailsCleanly(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ws, a, b := fixture(t, s)
	s.Close()
	done := make(chan error, 1)
	go func() {
		_, err := s.InsertMessage(ctx, ws, 0, a.ID, b.ID, "note", "late", 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("write after close reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("write after close hangs")
	}
}
