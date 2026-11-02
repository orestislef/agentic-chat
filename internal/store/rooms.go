package store

import (
	"context"
	"database/sql"
	"errors"
)

// Room is a workspace entered with a link.
type Room struct {
	ID          int64
	WorkspaceID int64
	UserID      int64
}

const (
	// maxRoomDevices caps how many machines can join one room.
	maxRoomDevices = 100
	// roomIdleLifetime is how long a room stays around without anyone using it.
	roomIdleLifetime = 90 * 24 * 60 * 60 * 1000
	roomTouchGap     = 60 * 60 * 1000
)

// CreateRoom stores a new room with its hidden owner. The owner has no membership and no agent,
// so it shows up nowhere, and its password can never match, so nobody can log in as it.
func (s *Store) CreateRoom(ctx context.Context, tokenHash, ownerName string) (*Room, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := nowMillis()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, is_admin, created_at, demo, room) VALUES (?, '!', 0, ?, ?, 1)`, ownerName, now, s.demo)
	if err != nil {
		return nil, err
	}
	userID, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO workspaces (name, created_at, demo) VALUES ('room', ?, ?)`, now, s.demo)
	if err != nil {
		return nil, err
	}
	workspaceID, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	res, err = tx.ExecContext(ctx,
		`INSERT INTO rooms (token_hash, workspace_id, user_id, created_at, last_active_at) VALUES (?, ?, ?, ?, ?)`,
		tokenHash, workspaceID, userID, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Room{ID: id, WorkspaceID: workspaceID, UserID: userID}, tx.Commit()
}

// RoomByToken finds a room by the hash of its link, and notes that it is still in use.
func (s *Store) RoomByToken(ctx context.Context, tokenHash string) (*Room, error) {
	var r Room
	var active int64
	err := s.db.QueryRowContext(ctx, `SELECT id, workspace_id, user_id, last_active_at FROM rooms WHERE token_hash = ?`, tokenHash).
		Scan(&r.ID, &r.WorkspaceID, &r.UserID, &active)
	if err != nil {
		return nil, rowErr(err)
	}
	if now := nowMillis(); now-active > roomTouchGap {
		if _, err := s.w.ExecContext(ctx, `UPDATE rooms SET last_active_at = ? WHERE id = ?`, now, r.ID); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

// EnsureRoomConnection makes sure a machine has its own connection in a room. Each machine counts
// as a separate party, so the same folder name on two machines stays two agents.
func (s *Store) EnsureRoomConnection(ctx context.Context, room *Room, tokenHash string) error {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM connections WHERE token_hash = ?`, tokenHash).Scan(&found)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		ctx := context.Background()
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM connections WHERE workspace_id = ?`, room.WorkspaceID).Scan(&n); err != nil {
			return err
		}
		if n >= maxRoomDevices {
			return ErrAgentLimit
		}
		now := nowMillis()
		_, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO connections (token_hash, client_name, user_id, workspace_id, created_at, last_used_at, expires_at, shared)
			VALUES (?, 'Room', ?, ?, ?, ?, ?, 1)`, tokenHash, room.UserID, room.WorkspaceID, now, now, now+connectionLifetime)
		return err
	})
}

// DeleteRoom removes a room with everything in it and its hidden owner.
func (s *Store) DeleteRoom(ctx context.Context, room *Room) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, room.WorkspaceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ? AND room = 1`, room.UserID); err != nil {
		return err
	}
	return tx.Commit()
}
