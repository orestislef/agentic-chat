// Package store keeps rooms, who is in them and what they said, in one SQLite file.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound means the room, member or session does not exist.
var ErrNotFound = errors.New("not found")

// ErrRoomFull means a room has as many members as it can take.
var ErrRoomFull = errors.New("room is full")

// MaxMembers caps how many names one room can have.
const MaxMembers = 50

const (
	roomIdle    = 90 * 24 * time.Hour // a room nobody uses is deleted after this
	sessionIdle = 30 * 24 * time.Hour
	touchGap    = 15 * time.Second // presence is written at most this often
)

// Store wraps the database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE rooms (
	id         INTEGER PRIMARY KEY,
	token_hash TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	active_at  INTEGER NOT NULL
);
-- A member is a name in a room. The name is the identity: whoever joins under it gets
-- its unread messages, so a chat that restarts picks up where it left off.
CREATE TABLE members (
	id        INTEGER PRIMARY KEY,
	room_id   INTEGER NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	name      TEXT NOT NULL,
	joined_at INTEGER NOT NULL,
	seen_at   INTEGER NOT NULL,
	read_id   INTEGER NOT NULL DEFAULT 0,
	UNIQUE (room_id, name)
);
-- Which member an MCP session speaks for.
CREATE TABLE sessions (
	id_hash   TEXT PRIMARY KEY,
	member_id INTEGER NOT NULL REFERENCES members(id) ON DELETE CASCADE,
	seen_at   INTEGER NOT NULL
);
CREATE INDEX sessions_member ON sessions(member_id);
-- One conversation per room. Names are copied in, so history stays readable after a member is removed.
CREATE TABLE messages (
	id         INTEGER PRIMARY KEY,
	room_id    INTEGER NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
	from_name  TEXT NOT NULL,
	to_name    TEXT NOT NULL DEFAULT '',
	body       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX messages_room ON messages(room_id, id);
`

// Open opens or creates the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+
		"?_pragma=busy_timeout(10000)"+
		"&_pragma=auto_vacuum(INCREMENTAL)"+ // only takes effect on a new file, has to come first
		"&_pragma=journal_mode(WAL)"+
		"&_pragma=synchronous(FULL)"+ // a message that was acknowledged survives a power cut
		"&_pragma=foreign_keys(1)"+
		"&_pragma=temp_store(MEMORY)"+ // the container has no writable /tmp
		"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("database %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	var check string
	if err := s.db.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&check); err != nil {
		return fmt.Errorf("cannot be read: %w", err)
	}
	if check != "ok" {
		return fmt.Errorf("is damaged: %s", check)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 1 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 1`); err != nil {
		return err
	}
	return tx.Commit()
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func now() int64 {
	return time.Now().UnixMilli()
}

func rowErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Room is a shared conversation entered with a link.
type Room struct {
	ID int64
}

// CreateRoom stores a room under the hash of its link token.
func (s *Store) CreateRoom(ctx context.Context, tokenHash string) (*Room, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO rooms (token_hash, created_at, active_at) VALUES (?, ?, ?)`, tokenHash, t, t)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Room{ID: id}, nil
}

// RoomByToken finds a room by the hash of its link token.
func (s *Store) RoomByToken(ctx context.Context, tokenHash string) (*Room, error) {
	var r Room
	var active int64
	err := s.db.QueryRowContext(ctx, `SELECT id, active_at FROM rooms WHERE token_hash = ?`, tokenHash).Scan(&r.ID, &active)
	if err != nil {
		return nil, rowErr(err)
	}
	if t := now(); t-active > time.Hour.Milliseconds() {
		if _, err := s.db.ExecContext(ctx, `UPDATE rooms SET active_at = ? WHERE id = ?`, t, r.ID); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

// DeleteRoom removes a room with everyone and everything in it.
func (s *Store) DeleteRoom(ctx context.Context, roomID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM rooms WHERE id = ?`, roomID)
	return err
}

// Member is a name in a room.
type Member struct {
	ID     int64  `json:"-"`
	RoomID int64  `json:"-"`
	Name   string `json:"name"`
	SeenAt int64  `json:"seen_at"`
	ReadID int64  `json:"-"`
}

const memberColumns = `id, room_id, name, seen_at, read_id`

func scanMember(row interface{ Scan(...any) error }) (*Member, error) {
	var m Member
	if err := row.Scan(&m.ID, &m.RoomID, &m.Name, &m.SeenAt, &m.ReadID); err != nil {
		return nil, rowErr(err)
	}
	return &m, nil
}

// Join makes a session speak as name in a room. An existing name is taken over with its unread
// messages. A new one starts reading from now, earlier messages are in the history.
func (s *Store) Join(ctx context.Context, roomID int64, sessionHash, name string) (m *Member, created bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	t := now()
	m, err = scanMember(tx.QueryRowContext(ctx, `SELECT `+memberColumns+` FROM members WHERE room_id = ? AND name = ?`, roomID, name))
	if errors.Is(err, ErrNotFound) {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM members WHERE room_id = ?`, roomID).Scan(&n); err != nil {
			return nil, false, err
		}
		if n >= MaxMembers {
			return nil, false, ErrRoomFull
		}
		var last int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM messages WHERE room_id = ?`, roomID).Scan(&last); err != nil {
			return nil, false, err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO members (room_id, name, joined_at, seen_at, read_id) VALUES (?, ?, ?, ?, ?)`, roomID, name, t, t, last)
		if err != nil {
			return nil, false, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, false, err
		}
		m = &Member{ID: id, RoomID: roomID, Name: name, SeenAt: t, ReadID: last}
		created = true
	} else if err != nil {
		return nil, false, err
	} else if _, err := tx.ExecContext(ctx, `UPDATE members SET seen_at = ? WHERE id = ?`, t, m.ID); err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (id_hash, member_id, seen_at) VALUES (?, ?, ?)
		ON CONFLICT(id_hash) DO UPDATE SET member_id = excluded.member_id, seen_at = excluded.seen_at`,
		sessionHash, m.ID, t); err != nil {
		return nil, false, err
	}
	return m, created, tx.Commit()
}

// SessionMember finds who a session speaks for, and notes that it is still around.
func (s *Store) SessionMember(ctx context.Context, sessionHash string) (*Member, error) {
	var seen int64
	var m Member
	err := s.db.QueryRowContext(ctx, `
		SELECT m.id, m.room_id, m.name, m.seen_at, m.read_id, s.seen_at
		FROM sessions s JOIN members m ON m.id = s.member_id WHERE s.id_hash = ?`, sessionHash).
		Scan(&m.ID, &m.RoomID, &m.Name, &m.SeenAt, &m.ReadID, &seen)
	if err != nil {
		return nil, rowErr(err)
	}
	t := now()
	if t-m.SeenAt > touchGap.Milliseconds() {
		if _, err := s.db.ExecContext(ctx, `UPDATE members SET seen_at = ? WHERE id = ?`, t, m.ID); err != nil {
			return nil, err
		}
		m.SeenAt = t
	}
	if t-seen > time.Hour.Milliseconds() {
		if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET seen_at = ? WHERE id_hash = ?`, t, sessionHash); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

// EndSession forgets a session, for clients that say goodbye. The member stays.
func (s *Store) EndSession(ctx context.Context, sessionHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, sessionHash)
	return err
}

// Members lists a room's members, by name.
func (s *Store) Members(ctx context.Context, roomID int64) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+memberColumns+` FROM members WHERE room_id = ? ORDER BY name`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// RemoveMember takes a name out of a room. A chat still using it is out of the room until it joins again.
func (s *Store) RemoveMember(ctx context.Context, roomID int64, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM members WHERE room_id = ? AND name = ?`, roomID, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Message is one thing said in a room. To is empty when it is meant for everyone.
type Message struct {
	ID   int64  `json:"id"`
	From string `json:"from"`
	To   string `json:"to,omitempty"`
	Body string `json:"text"`
	At   int64  `json:"at"`
}

const messageColumns = `id, from_name, to_name, body, created_at`

func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.From, &m.To, &m.Body, &m.At); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Post adds a message to a room.
func (s *Store) Post(ctx context.Context, roomID int64, from, to, body string) (*Message, error) {
	m := &Message{From: from, To: to, Body: body, At: now()}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (room_id, from_name, to_name, body, created_at) VALUES (?, ?, ?, ?, ?)`, roomID, from, to, body, m.At)
	if err != nil {
		return nil, err
	}
	if m.ID, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	return m, nil
}

// Unread returns what others said since a member last read, oldest first, and moves the member's
// read position past it.
func (s *Store) Unread(ctx context.Context, m *Member, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM messages
		WHERE room_id = ? AND id > ? AND from_name <> ? ORDER BY id LIMIT ?`, m.RoomID, m.ReadID, m.Name, limit)
	if err != nil {
		return nil, err
	}
	out, err := scanMessages(rows)
	if err != nil || len(out) == 0 {
		return out, err
	}
	last := out[len(out)-1].ID
	if _, err := s.db.ExecContext(ctx, `UPDATE members SET read_id = ? WHERE id = ? AND read_id < ?`, last, m.ID, last); err != nil {
		return nil, err
	}
	m.ReadID = last
	return out, nil
}

// CountUnread counts what others said since a member last read, and how much of it names the member.
func (s *Store) CountUnread(ctx context.Context, m *Member) (all, forMe int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(to_name = ?), 0) FROM messages
		WHERE room_id = ? AND id > ? AND from_name <> ?`, m.Name, m.RoomID, m.ReadID, m.Name).Scan(&all, &forMe)
	return all, forMe, err
}

// History returns up to limit messages before beforeID, or the newest when it is 0, oldest first.
func (s *Store) History(ctx context.Context, roomID, beforeID int64, limit int) ([]Message, error) {
	query := `SELECT ` + messageColumns + ` FROM messages WHERE room_id = ?`
	args := []any{roomID}
	if beforeID > 0 {
		query += ` AND id < ?`
		args = append(args, beforeID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	out, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// After returns up to limit messages after afterID, oldest first.
func (s *Store) After(ctx context.Context, roomID, afterID int64, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM messages
		WHERE room_id = ? AND id > ? ORDER BY id LIMIT ?`, roomID, afterID, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// Search finds messages in a room that contain all of the words, newest first.
func (s *Store) Search(ctx context.Context, roomID int64, words []string, limit int) ([]Message, error) {
	query := `SELECT ` + messageColumns + ` FROM messages WHERE room_id = ?`
	args := []any{roomID}
	for _, w := range words {
		query += ` AND body LIKE ? ESCAPE '\'`
		args = append(args, "%"+likeEscaper.Replace(w)+"%")
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// Clean deletes rooms nobody used for a long time and sessions that went quiet, then gives
// freed space back to the disk. It is meant to run about once an hour.
func (s *Store) Clean(ctx context.Context) error {
	t := now()
	steps := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM rooms WHERE active_at < ?`, []any{t - roomIdle.Milliseconds()}},
		{`DELETE FROM sessions WHERE seen_at < ?`, []any{t - sessionIdle.Milliseconds()}},
		{`PRAGMA optimize`, nil},
		{`PRAGMA incremental_vacuum`, nil},
		{`PRAGMA wal_checkpoint(TRUNCATE)`, nil},
	}
	for _, step := range steps {
		if _, err := s.db.ExecContext(ctx, step.query, step.args...); err != nil {
			return fmt.Errorf("%s: %w", step.query, err)
		}
	}
	return nil
}
