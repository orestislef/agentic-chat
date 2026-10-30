package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// User is an account that can log in to the web UI.
type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	IsAdmin      bool   `json:"is_admin"`
	CreatedAt    int64  `json:"created_at"`
	PasswordHash string `json:"-"`
}

// Workspace groups agents that are allowed to talk to each other.
type Workspace struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Role      string `json:"role,omitempty"`
	Demo      bool   `json:"demo,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// Member is a user who belongs to a workspace.
type Member struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// Invite lets a new or existing user join a workspace. The code itself is only
// ever shown once, the database keeps its hash.
type Invite struct {
	ID          int64 `json:"id"`
	WorkspaceID int64 `json:"workspace_id"`
	MaxUses     int   `json:"max_uses"`
	Uses        int   `json:"uses"`
	CreatedAt   int64 `json:"created_at"`
	ExpiresAt   int64 `json:"expires_at"`
}

// CountUsers reports how many accounts exist.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateAccount creates a user and either joins the workspace behind inviteHash
// or, when inviteHash is empty, creates a personal workspace. The first account
// on a hub becomes the admin. All of it happens in one transaction so two
// simultaneous first registrations cannot both become admin.
func (s *Store) CreateAccount(ctx context.Context, username, passwordHash, inviteHash, workspaceName string) (*User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&existing); err != nil {
		return nil, err
	}
	now := nowMillis()
	u := &User{Username: username, IsAdmin: existing == 0, CreatedAt: now}
	demo := s.demo && !u.IsAdmin
	res, err := tx.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, is_admin, created_at, demo) VALUES (?, ?, ?, ?, ?)`,
		username, passwordHash, u.IsAdmin, now, demo)
	if err != nil {
		if isUniqueErr(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	if u.ID, err = res.LastInsertId(); err != nil {
		return nil, err
	}

	if inviteHash != "" {
		if _, err := redeemInvite(ctx, tx, inviteHash, u.ID, username); err != nil {
			return nil, err
		}
	} else {
		if _, err := createWorkspace(ctx, tx, workspaceName, u.ID, username, demo); err != nil {
			return nil, err
		}
	}
	return u, tx.Commit()
}

// UserByUsername finds an account by name, case insensitive.
func (s *Store) UserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, is_admin, created_at FROM users WHERE username = ?`, username))
}

// UserByID finds an account by id.
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, is_admin, created_at FROM users WHERE id = ?`, id))
}

func scanUser(row *sql.Row) (*User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.IsAdmin, &u.CreatedAt); err != nil {
		return nil, rowErr(err)
	}
	return &u, nil
}

// SetPassword replaces the password and logs the user out everywhere, except
// for keepSession when it is not empty.
func (s *Store) SetPassword(ctx context.Context, userID int64, passwordHash, keepSession string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, keepSession); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateSession stores a login session.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, userID, nowMillis(), expiresAt)
	return err
}

// UserBySession resolves a session token hash to its user if it has not expired.
func (s *Store) UserBySession(ctx context.Context, tokenHash string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `
		SELECT u.id, u.username, u.password_hash, u.is_admin, u.created_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, tokenHash, nowMillis()))
}

// DeleteSession ends a login session.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// CreateWorkspace creates a workspace owned by the user.
func (s *Store) CreateWorkspace(ctx context.Context, name string, user *User) (*Workspace, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	w, err := createWorkspace(ctx, tx, name, user.ID, user.Username, s.demo && !user.IsAdmin)
	if err != nil {
		return nil, err
	}
	return w, tx.Commit()
}

func createWorkspace(ctx context.Context, tx *sql.Tx, name string, userID int64, username string, demo bool) (*Workspace, error) {
	now := nowMillis()
	res, err := tx.ExecContext(ctx, `INSERT INTO workspaces (name, created_at, demo) VALUES (?, ?, ?)`, name, now, demo)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	if err := addMember(ctx, tx, id, userID, username, "owner"); err != nil {
		return nil, err
	}
	return &Workspace{ID: id, Name: name, Role: "owner", Demo: demo, CreatedAt: now}, nil
}

// addMember adds the membership and the human agent row that represents the
// person in this workspace. A taken name gets a numeric suffix.
func addMember(ctx context.Context, tx *sql.Tx, workspaceID, userID int64, username, role string) error {
	now := nowMillis()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memberships (workspace_id, user_id, role, created_at) VALUES (?, ?, ?, ?)`,
		workspaceID, userID, role, now); err != nil {
		if isUniqueErr(err) {
			return ErrConflict
		}
		return err
	}
	base := strings.ToLower(username)
	for i := 1; i < 100; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO agents (workspace_id, user_id, name, kind, created_at) VALUES (?, ?, ?, 'human', ?)`,
			workspaceID, userID, name, now)
		if err == nil {
			return nil
		}
		if !isUniqueErr(err) {
			return err
		}
	}
	return ErrConflict
}

// WorkspacesForUser lists the workspaces a user belongs to.
func (s *Store) WorkspacesForUser(ctx context.Context, userID int64) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT w.id, w.name, m.role, w.demo, w.created_at
		FROM workspaces w JOIN memberships m ON m.workspace_id = w.id
		WHERE m.user_id = ? ORDER BY w.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Workspace{}
	for rows.Next() {
		var w Workspace
		if err := rows.Scan(&w.ID, &w.Name, &w.Role, &w.Demo, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// MemberRole returns the user's role in a workspace, or ErrNotFound if they are not a member.
func (s *Store) MemberRole(ctx context.Context, workspaceID, userID int64) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx,
		`SELECT role FROM memberships WHERE workspace_id = ? AND user_id = ?`, workspaceID, userID).Scan(&role)
	return role, rowErr(err)
}

// Members lists the people in a workspace.
func (s *Store) Members(ctx context.Context, workspaceID int64) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.username, m.role
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.workspace_id = ? ORDER BY m.created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Username, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RemoveMember removes a person from a workspace and revokes their agents there.
// The last owner cannot be removed, otherwise nobody could manage the workspace.
func (s *Store) RemoveMember(ctx context.Context, workspaceID, userID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role string
	if err := tx.QueryRowContext(ctx,
		`SELECT role FROM memberships WHERE workspace_id = ? AND user_id = ?`, workspaceID, userID).Scan(&role); err != nil {
		return rowErr(err)
	}
	if role == "owner" {
		var owners int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memberships WHERE workspace_id = ? AND role = 'owner'`, workspaceID).Scan(&owners); err != nil {
			return err
		}
		if owners <= 1 {
			return ErrLastOwner
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, workspaceID, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE agents SET revoked_at = ?, token_hash = NULL WHERE workspace_id = ? AND user_id = ? AND revoked_at IS NULL`,
		nowMillis(), workspaceID, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrLastOwner protects a workspace from losing its only owner.
var ErrLastOwner = errors.New("workspace needs at least one owner")

// ErrInviteInvalid covers unknown, expired and used up invite codes alike, so a
// caller cannot probe which codes once existed.
var ErrInviteInvalid = errors.New("invite is not valid")

// CreateInvite stores a new invite code hash.
func (s *Store) CreateInvite(ctx context.Context, workspaceID, createdBy int64, codeHash string, maxUses int, expiresAt int64) (*Invite, error) {
	now := nowMillis()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO invites (workspace_id, code_hash, created_by, max_uses, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, workspaceID, codeHash, createdBy, maxUses, now, expiresAt)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Invite{ID: id, WorkspaceID: workspaceID, MaxUses: maxUses, CreatedAt: now, ExpiresAt: expiresAt}, nil
}

// Invites lists the invites of a workspace that can still be used.
func (s *Store) Invites(ctx context.Context, workspaceID int64) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, workspace_id, max_uses, uses, created_at, expires_at FROM invites
		WHERE workspace_id = ? AND expires_at > ? AND uses < max_uses ORDER BY created_at DESC`,
		workspaceID, nowMillis())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Invite{}
	for rows.Next() {
		var i Invite
		if err := rows.Scan(&i.ID, &i.WorkspaceID, &i.MaxUses, &i.Uses, &i.CreatedAt, &i.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// DeleteInvite removes an invite from a workspace.
func (s *Store) DeleteInvite(ctx context.Context, workspaceID, inviteID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM invites WHERE id = ? AND workspace_id = ?`, inviteID, workspaceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AcceptInvite adds an existing user to the workspace behind the invite code.
func (s *Store) AcceptInvite(ctx context.Context, codeHash string, user *User) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	id, err := redeemInvite(ctx, tx, codeHash, user.ID, user.Username)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func redeemInvite(ctx context.Context, tx *sql.Tx, codeHash string, userID int64, username string) (int64, error) {
	var inviteID, workspaceID int64
	err := tx.QueryRowContext(ctx,
		`SELECT id, workspace_id FROM invites WHERE code_hash = ? AND expires_at > ? AND uses < max_uses`,
		codeHash, nowMillis()).Scan(&inviteID, &workspaceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrInviteInvalid
		}
		return 0, err
	}
	if err := addMember(ctx, tx, workspaceID, userID, username, "member"); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invites SET uses = uses + 1 WHERE id = ?`, inviteID); err != nil {
		return 0, err
	}
	return workspaceID, nil
}

func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
