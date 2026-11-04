package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "rooms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSameNameIsTheSameMemberWithItsUnreadMessages(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	room, err := s.CreateRoom(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	backend, created, err := s.Join(ctx, room.ID, "session-a", "backend")
	if err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	app, _, _ := s.Join(ctx, room.ID, "session-b", "app")
	if _, err := s.Post(ctx, room.ID, app.Name, "backend", "which endpoint?"); err != nil {
		t.Fatal(err)
	}

	// The chat restarts: new session, same name.
	again, created, err := s.Join(ctx, room.ID, "session-c", "backend")
	if err != nil || created || again.ID != backend.ID {
		t.Fatalf("restart got a different member: %v %v %v", again, created, err)
	}
	got, err := s.Unread(ctx, again, 10)
	if err != nil || len(got) != 1 || got[0].Body != "which endpoint?" || got[0].To != "backend" {
		t.Fatalf("%v %v", got, err)
	}
	if more, _ := s.Unread(ctx, again, 10); len(more) != 0 {
		t.Fatalf("read twice: %v", more)
	}
	// The old session still speaks for the same name.
	if m, err := s.SessionMember(ctx, "session-a"); err != nil || m.ID != backend.ID {
		t.Fatalf("%v %v", m, err)
	}
}

func TestNewMemberStartsAtNowAndOwnMessagesAreNotUnread(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	room, _ := s.CreateRoom(ctx, "h1")
	a, _, _ := s.Join(ctx, room.ID, "a", "a")
	s.Post(ctx, room.ID, "a", "", "before b joined")
	b, _, _ := s.Join(ctx, room.ID, "b", "b")
	if got, _ := s.Unread(ctx, b, 10); len(got) != 0 {
		t.Fatalf("a newcomer got old messages as new: %v", got)
	}
	s.Post(ctx, room.ID, "b", "", "hello")
	s.Post(ctx, room.ID, "a", "b", "hi b")
	all, forMe, err := s.CountUnread(ctx, b)
	if err != nil || all != 1 || forMe != 1 {
		t.Fatalf("%d %d %v", all, forMe, err)
	}
	got, _ := s.Unread(ctx, a, 10)
	if len(got) != 1 || got[0].Body != "hello" {
		t.Fatalf("%v", got)
	}
	history, _ := s.History(ctx, room.ID, 0, 10)
	if len(history) != 3 || history[0].Body != "before b joined" {
		t.Fatalf("%v", history)
	}
}

func TestRoomsAreSeparate(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	one, _ := s.CreateRoom(ctx, "one")
	two, _ := s.CreateRoom(ctx, "two")
	s.Join(ctx, one.ID, "x", "backend")
	b2, _, _ := s.Join(ctx, two.ID, "y", "backend")
	s.Post(ctx, one.ID, "backend", "", "only in one")
	if got, _ := s.Unread(ctx, b2, 10); len(got) != 0 {
		t.Fatalf("leaked across rooms: %v", got)
	}
	if got, _ := s.Search(ctx, two.ID, []string{"only"}, 10); len(got) != 0 {
		t.Fatalf("search leaked: %v", got)
	}
	if got, _ := s.Search(ctx, one.ID, []string{"ONLY", "one"}, 10); len(got) != 1 {
		t.Fatalf("search: %v", got)
	}
	if got, _ := s.Search(ctx, one.ID, []string{"%"}, 10); len(got) != 0 {
		t.Fatalf("wildcards are not escaped: %v", got)
	}
}

func TestRemovingAndDeleting(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	room, _ := s.CreateRoom(ctx, "h1")
	s.Join(ctx, room.ID, "a", "backend")
	s.Post(ctx, room.ID, "backend", "", "said before")
	if err := s.RemoveMember(ctx, room.ID, "backend"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionMember(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed chat still in: %v", err)
	}
	if h, _ := s.History(ctx, room.ID, 0, 10); len(h) != 1 {
		t.Fatal("history lost with the member")
	}
	if err := s.RemoveMember(ctx, room.ID, "backend"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	s.Join(ctx, room.ID, "b", "app")
	if err := s.DeleteRoom(ctx, room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RoomByToken(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.SessionMember(ctx, "b"); !errors.Is(err, ErrNotFound) {
		t.Fatal("session outlived its room")
	}
	if h, _ := s.History(ctx, room.ID, 0, 10); len(h) != 0 {
		t.Fatal("messages outlived their room")
	}
}

func TestRoomFull(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	room, _ := s.CreateRoom(ctx, "h1")
	for i := range MaxMembers {
		if _, _, err := s.Join(ctx, room.ID, fmt.Sprint("s", i), fmt.Sprint("m", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Join(ctx, room.ID, "extra", "extra"); !errors.Is(err, ErrRoomFull) {
		t.Fatal(err)
	}
	// An existing name still gets in.
	if _, _, err := s.Join(ctx, room.ID, "again", "m0"); err != nil {
		t.Fatal(err)
	}
}

func TestReopenKeepsEverythingAndCleanRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rooms.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	room, _ := s.CreateRoom(ctx, "h1")
	s.Join(ctx, room.ID, "a", "backend")
	s.Post(ctx, room.ID, "backend", "", "kept")
	if err := s.Clean(ctx); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if m, err := s.SessionMember(ctx, "a"); err != nil || m.Name != "backend" {
		t.Fatalf("%v %v", m, err)
	}
}
