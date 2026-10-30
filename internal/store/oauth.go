package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// OAuthClient is a program that registered itself to sign users in.
type OAuthClient struct {
	ID           string
	Name         string
	RedirectURIs []string
}

// OAuthCode is what a user's approval turns into, on its way to becoming a connection.
type OAuthCode struct {
	ClientID    string
	UserID      int64
	WorkspaceID int64
	RedirectURI string
	Challenge   string
}

// Connection is one signed in MCP client. It can stand behind many chats.
type Connection struct {
	ID          int64  `json:"id"`
	ClientName  string `json:"client_name"`
	UserID      int64  `json:"-"`
	WorkspaceID int64  `json:"workspace_id"`
	Workspace   string `json:"workspace,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	LastUsedAt  int64  `json:"last_used_at"`
	ExpiresAt   int64  `json:"-"`
}

// ErrAgentLimit means a workspace cannot take another agent.
var ErrAgentLimit = errors.New("this workspace has reached its limit of agents")

const (
	// joinActiveWindow is how long a chat keeps its name to itself since its last call. A hub
	// cannot tell a closed chat from a quiet one, and most clients leave without saying so.
	// It only has to outlast the longest blocking call, so a listening chat stays busy.
	joinActiveWindow = 2 * 60 * 1000
	// connectionLifetime slides forward with use, so only idle connections expire.
	connectionLifetime = 90 * 24 * 60 * 60 * 1000
	connectionTouchGap = 60 * 60 * 1000
	sessionTouchGap    = 10 * 1000
)

// CreateOAuthClient stores a client registration.
func (s *Store) CreateOAuthClient(ctx context.Context, c OAuthClient) error {
	uris, err := json.Marshal(c.RedirectURIs)
	if err != nil {
		return err
	}
	_, err = s.w.ExecContext(ctx, `INSERT INTO oauth_clients (id, name, redirect_uris, created_at) VALUES (?, ?, ?, ?)`,
		c.ID, c.Name, string(uris), nowMillis())
	return err
}

// OAuthClientByID finds a registered client.
func (s *Store) OAuthClientByID(ctx context.Context, id string) (*OAuthClient, error) {
	c := OAuthClient{ID: id}
	var uris string
	if err := s.db.QueryRowContext(ctx, `SELECT name, redirect_uris FROM oauth_clients WHERE id = ?`, id).Scan(&c.Name, &uris); err != nil {
		return nil, rowErr(err)
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return nil, err
	}
	return &c, nil
}

// CreateOAuthCode stores an approval for a few minutes.
func (s *Store) CreateOAuthCode(ctx context.Context, codeHash string, c OAuthCode, expiresAt int64) error {
	_, err := s.w.ExecContext(ctx, `
		INSERT INTO oauth_codes (code_hash, client_id, user_id, workspace_id, redirect_uri, challenge, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, codeHash, c.ClientID, c.UserID, c.WorkspaceID, c.RedirectURI, c.Challenge, expiresAt)
	return err
}

// ConsumeOAuthCode returns an approval and deletes it, so it can only ever be used once,
// even if the attempt that uses it turns out to be wrong.
func (s *Store) ConsumeOAuthCode(ctx context.Context, codeHash string) (*OAuthCode, error) {
	var c OAuthCode
	err := s.write(ctx, func(tx *sql.Tx) error {
		ctx := context.Background()
		err := tx.QueryRowContext(ctx, `
			SELECT client_id, user_id, workspace_id, redirect_uri, challenge FROM oauth_codes
			WHERE code_hash = ? AND expires_at > ?`, codeHash, nowMillis()).
			Scan(&c.ClientID, &c.UserID, &c.WorkspaceID, &c.RedirectURI, &c.Challenge)
		if err != nil {
			return rowErr(err)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM oauth_codes WHERE code_hash = ?`, codeHash)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// CreateConnection stores a signed in client.
func (s *Store) CreateConnection(ctx context.Context, tokenHash, clientName string, userID, workspaceID int64) error {
	now := nowMillis()
	_, err := s.w.ExecContext(ctx, `
		INSERT INTO connections (token_hash, client_name, user_id, workspace_id, created_at, last_used_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, tokenHash, clientName, userID, workspaceID, now, now, now+connectionLifetime)
	return err
}

// ConnectionByToken resolves a token and keeps the connection alive.
func (s *Store) ConnectionByToken(ctx context.Context, tokenHash string) (*Connection, error) {
	var c Connection
	now := nowMillis()
	err := s.db.QueryRowContext(ctx, `
		SELECT id, client_name, user_id, workspace_id, created_at, last_used_at, expires_at
		FROM connections WHERE token_hash = ? AND expires_at > ?`, tokenHash, now).
		Scan(&c.ID, &c.ClientName, &c.UserID, &c.WorkspaceID, &c.CreatedAt, &c.LastUsedAt, &c.ExpiresAt)
	if err != nil {
		return nil, rowErr(err)
	}
	if now-c.LastUsedAt > connectionTouchGap {
		if _, err := s.w.ExecContext(ctx, `UPDATE connections SET last_used_at = ?, expires_at = ? WHERE id = ?`, now, now+connectionLifetime, c.ID); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

// ConnectionsForUser lists what a person has signed in.
func (s *Store) ConnectionsForUser(ctx context.Context, userID int64) ([]Connection, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.client_name, c.user_id, c.workspace_id, w.name, c.created_at, c.last_used_at, c.expires_at
		FROM connections c JOIN workspaces w ON w.id = c.workspace_id
		WHERE c.user_id = ? AND c.expires_at > ? ORDER BY c.last_used_at DESC`, userID, nowMillis())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Connection{}
	for rows.Next() {
		var c Connection
		if err := rows.Scan(&c.ID, &c.ClientName, &c.UserID, &c.WorkspaceID, &c.Workspace, &c.CreatedAt, &c.LastUsedAt, &c.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteConnection signs a client out. Only its owner can do that.
func (s *Store) DeleteConnection(ctx context.Context, userID, connectionID int64) error {
	res, err := s.w.ExecContext(ctx, `DELETE FROM connections WHERE id = ? AND user_id = ?`, connectionID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteMemberConnections signs a removed member's clients out of a workspace.
func (s *Store) DeleteMemberConnections(ctx context.Context, workspaceID, userID int64) error {
	_, err := s.w.ExecContext(ctx, `DELETE FROM connections WHERE workspace_id = ? AND user_id = ?`, workspaceID, userID)
	return err
}

// ChatState is what the hub knows about one chat.
type ChatState struct {
	Agent *Agent // nil if it has none yet, or lost it
	// Removed means its agent was taken away in the web UI.
	Removed bool
	// Wanted is the name it asked for, Agent may have a suffix while that one is busy.
	Wanted string
}

// SessionAgent finds the agent of a chat.
func (s *Store) SessionAgent(ctx context.Context, sessionHash string) (ChatState, error) {
	var state ChatState
	var agentID sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT agent_id, wanted_name FROM chat_sessions WHERE session_hash = ?`, sessionHash).Scan(&agentID, &state.Wanted)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatState{}, nil
	}
	if err != nil {
		return ChatState{}, err
	}
	if !agentID.Valid {
		state.Removed = true
		return state, nil
	}
	a, err := scanAgent(s.db.QueryRowContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE id = ? AND revoked_at IS NULL`, agentID.Int64))
	if errors.Is(err, ErrNotFound) {
		state.Removed = true
		return state, nil
	}
	if err != nil {
		return ChatState{}, err
	}
	state.Agent = a
	return state, nil
}

// RetireChatAgent moves what is waiting for a stand-in name over to the real one, then
// removes the stand-in. The old name stays readable in history.
func (s *Store) RetireChatAgent(ctx context.Context, from, to *Agent) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		ctx := context.Background()
		if _, err := tx.ExecContext(ctx,
			`UPDATE messages SET to_agent_id = ? WHERE to_agent_id = ? AND read_at IS NULL`, to.ID, from.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE agents SET revoked_at = ?, token_hash = NULL
			WHERE id = ? AND auto = 1 AND revoked_at IS NULL
			AND NOT EXISTS (SELECT 1 FROM chat_sessions WHERE agent_id = agents.id AND last_seen_at > ?)`,
			nowMillis(), from.ID, nowMillis()-joinActiveWindow)
		return err
	})
}

// TouchSession notes that a chat is still around. It only writes every few seconds.
func (s *Store) TouchSession(ctx context.Context, sessionHash string) error {
	now := nowMillis()
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(),
			`UPDATE chat_sessions SET last_seen_at = ? WHERE session_hash = ? AND last_seen_at < ?`, now, sessionHash, now-sessionTouchGap)
		return err
	})
}

// EndSession frees a chat's name right away, for clients that say goodbye.
func (s *Store) EndSession(ctx context.Context, sessionHash string) error {
	_, err := s.w.ExecContext(ctx, `UPDATE chat_sessions SET last_seen_at = 0 WHERE session_hash = ?`, sessionHash)
	return err
}

// JoinAgent gives a chat its agent. A name of this user that no other chat is using gets
// picked up again, with its inbox and history, so reopening a project keeps its identity.
// Otherwise the next free name is taken: backend, backend-2 and so on. Maximum 0 means no cap.
func (s *Store) JoinAgent(ctx context.Context, conn *Connection, sessionHash, name string, maxAgents int) (*Agent, bool, error) {
	var agent *Agent
	created := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		ctx := context.Background()
		now := nowMillis()
		for i := 1; i <= 100; i++ {
			candidate := name
			if i > 1 {
				suffix := fmt.Sprintf("-%d", i)
				candidate = name[:min(len(name), 32-len(suffix))] + suffix
			}
			existing, err := scanAgent(tx.QueryRowContext(ctx,
				`SELECT `+agentColumns+` FROM agents WHERE workspace_id = ? AND name = ? AND revoked_at IS NULL`, conn.WorkspaceID, candidate))
			if errors.Is(err, ErrNotFound) {
				if maxAgents > 0 {
					var n int
					if err := tx.QueryRowContext(ctx,
						`SELECT COUNT(*) FROM agents WHERE workspace_id = ? AND kind = 'agent' AND revoked_at IS NULL`, conn.WorkspaceID).Scan(&n); err != nil {
						return err
					}
					if n >= maxAgents {
						return ErrAgentLimit
					}
				}
				res, err := tx.ExecContext(ctx, `
					INSERT INTO agents (workspace_id, user_id, name, kind, created_at, auto) VALUES (?, ?, ?, 'agent', ?, 1)`,
					conn.WorkspaceID, conn.UserID, candidate, now)
				if err != nil {
					return err
				}
				id, err := res.LastInsertId()
				if err != nil {
					return err
				}
				if agent, err = scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentColumns+` FROM agents WHERE id = ?`, id)); err != nil {
					return err
				}
				created = true
				return attachSession(ctx, tx, sessionHash, conn.ID, agent.ID, name, now)
			}
			if err != nil {
				return err
			}
			if existing.UserID != conn.UserID || existing.Kind != "agent" {
				continue
			}
			var busy int
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM chat_sessions WHERE agent_id = ? AND session_hash <> ? AND last_seen_at > ?`,
				existing.ID, sessionHash, now-joinActiveWindow).Scan(&busy); err != nil {
				return err
			}
			if busy > 0 {
				continue
			}
			agent = existing
			return attachSession(ctx, tx, sessionHash, conn.ID, agent.ID, name, now)
		}
		return ErrConflict
	})
	if err != nil {
		return nil, false, err
	}
	return agent, created, nil
}

func attachSession(ctx context.Context, tx *sql.Tx, sessionHash string, connectionID, agentID int64, wanted string, now int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO chat_sessions (session_hash, connection_id, agent_id, wanted_name, last_seen_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(session_hash) DO UPDATE SET agent_id = excluded.agent_id, wanted_name = excluded.wanted_name, last_seen_at = excluded.last_seen_at`,
		sessionHash, connectionID, agentID, wanted, now)
	return err
}

// CountUnread counts what an agent has not read yet.
func (s *Store) CountUnread(ctx context.Context, agentID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE to_agent_id = ? AND read_at IS NULL`, agentID).Scan(&n)
	return n, err
}
