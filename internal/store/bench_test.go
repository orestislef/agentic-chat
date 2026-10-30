package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// seeded builds a workspace with a realistic history: threads of a few messages, most of
// them old and read, some delivered but never marked read, a few still waiting.
type seeded struct {
	store       *Store
	workspaceID int64
	a, b        *Agent
	threadID    int64
}

func seedHistory(b *testing.B, messages int) *seeded {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	ctx := context.Background()
	u, err := s.CreateAccount(ctx, "owner", "h", "", "team")
	if err != nil {
		b.Fatal(err)
	}
	list, _ := s.WorkspacesForUser(ctx, u.ID)
	ws := list[0].ID
	a, _ := s.CreateAgent(ctx, ws, u.ID, "frontend", "", "ta")
	c, _ := s.CreateAgent(ctx, ws, u.ID, "backend", "", "tb")

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO messages (workspace_id, thread_id, from_agent_id, to_agent_id, kind, body, created_at, delivered_at, read_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().UnixMilli()
	var thread int64 = 1
	for i := 1; i <= messages; i++ {
		if i%5 == 1 {
			thread = int64(i)
		}
		from, to := a.ID, c.ID
		if i%2 == 0 {
			from, to = c.ID, a.ID
		}
		var delivered, read any = now, now
		switch {
		case i%10 == 0: // pushed but never marked read, which is normal in channel mode
			read = nil
		case i > messages-20: // still waiting for delivery
			delivered, read = nil, nil
		}
		body := fmt.Sprintf("message %d about pagination, caching and the orders endpoint, with a bit of context so it has a realistic size for what agents send to each other", i)
		if _, err := stmt.ExecContext(ctx, ws, thread, from, to, "note", body, now-int64(messages-i)*1000, delivered, read); err != nil {
			b.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return &seeded{store: s, workspaceID: ws, a: a, b: c, threadID: thread}
}

const benchMessages = 100_000

func BenchmarkInsertMessage(b *testing.B) {
	d := seedHistory(b, benchMessages)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := d.store.InsertMessage(ctx, d.workspaceID, 0, d.a.ID, d.b.ID, "note", "a new message with a realistic length for an agent", 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueries(b *testing.B) {
	d := seedHistory(b, benchMessages)
	ctx := context.Background()
	cases := []struct {
		name string
		run  func() error
	}{
		{"Threads", func() error { _, err := d.store.Threads(ctx, d.workspaceID, 0, 50); return err }},
		{"HistoryThread", func() error {
			_, err := d.store.History(ctx, d.workspaceID, HistoryFilter{ThreadID: d.threadID, Limit: 500})
			return err
		}},
		{"HistoryLatest", func() error { _, err := d.store.History(ctx, d.workspaceID, HistoryFilter{Limit: 100}); return err }},
		{"Search", func() error { _, err := d.store.Search(ctx, d.workspaceID, "pagination orders", 50); return err }},
		{"Unread", func() error { _, err := d.store.Unread(ctx, d.b.ID, 20); return err }},
		{"Undelivered", func() error { _, err := d.store.Undelivered(ctx, d.b.ID, 128); return err }},
		{"ThreadLength", func() error { _, err := d.store.ThreadLength(ctx, d.workspaceID, d.threadID); return err }},
		{"AgentByToken", func() error { _, err := d.store.AgentByToken(ctx, "ta"); return err }},
		{"Agents", func() error { _, err := d.store.Agents(ctx, d.workspaceID); return err }},
		{"UnansweredAsks", func() error { _, err := d.store.UnansweredAsks(ctx, d.b.ID, 200); return err }},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				if err := c.run(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkOpenWithIntegrityCheck(b *testing.B) {
	d := seedHistory(b, benchMessages)
	path := filepath.Join(b.TempDir(), "copy.db")
	if err := d.store.Backup(context.Background(), path); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		s, err := Open(path)
		if err != nil {
			b.Fatal(err)
		}
		s.Close()
	}
}
