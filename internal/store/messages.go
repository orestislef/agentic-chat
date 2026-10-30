package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Agent is a participant in a workspace, either a coding session or a person.
type Agent struct {
	ID          int64  `json:"id"`
	WorkspaceID int64  `json:"workspace_id"`
	UserID      int64  `json:"user_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	LastSeenAt  int64  `json:"last_seen_at,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

// Message is one entry in the history of a workspace.
type Message struct {
	ID          int64  `json:"id"`
	WorkspaceID int64  `json:"workspace_id"`
	ThreadID    int64  `json:"thread_id"`
	From        string `json:"from"`
	To          string `json:"to"`
	FromID      int64  `json:"from_id"`
	ToID        int64  `json:"to_id"`
	Kind        string `json:"kind"`
	Body        string `json:"body"`
	InReplyTo   int64  `json:"in_reply_to,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	DeliveredAt int64  `json:"delivered_at,omitempty"`
	ReadAt      int64  `json:"read_at,omitempty"`
}

// Thread summarizes a conversation for the history list.
type Thread struct {
	ID           int64    `json:"id"`
	Participants []string `json:"participants"`
	Preview      string   `json:"preview"`
	Messages     int      `json:"messages"`
	LastAt       int64    `json:"last_at"`
}

const agentColumns = `id, workspace_id, user_id, name, description, kind, last_seen_at, created_at`

func scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	var a Agent
	var seen sql.NullInt64
	if err := row.Scan(&a.ID, &a.WorkspaceID, &a.UserID, &a.Name, &a.Description, &a.Kind, &seen, &a.CreatedAt); err != nil {
		return nil, rowErr(err)
	}
	a.LastSeenAt = nullMillis(seen)
	return &a, nil
}

// CreateAgent adds a coding agent to a workspace.
func (s *Store) CreateAgent(ctx context.Context, workspaceID, userID int64, name, description, tokenHash string) (*Agent, error) {
	now := nowMillis()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO agents (workspace_id, user_id, name, description, kind, token_hash, created_at)
		VALUES (?, ?, ?, ?, 'agent', ?, ?)`, workspaceID, userID, name, description, tokenHash, now)
	if err != nil {
		if isUniqueErr(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Agent{ID: id, WorkspaceID: workspaceID, UserID: userID, Name: name, Description: description, Kind: "agent", CreatedAt: now}, nil
}

// AgentByToken resolves a token hash to its active agent.
func (s *Store) AgentByToken(ctx context.Context, tokenHash string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE token_hash = ? AND revoked_at IS NULL`, tokenHash))
}

// AgentByID finds an active agent in a workspace.
func (s *Store) AgentByID(ctx context.Context, workspaceID, agentID int64) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE id = ? AND workspace_id = ? AND revoked_at IS NULL`, agentID, workspaceID))
}

// AgentByName finds an active agent in a workspace by name.
func (s *Store) AgentByName(ctx context.Context, workspaceID int64, name string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE workspace_id = ? AND name = ? AND revoked_at IS NULL`, workspaceID, name))
}

// HumanAgent returns the row that represents a person in a workspace.
func (s *Store) HumanAgent(ctx context.Context, workspaceID, userID int64) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE workspace_id = ? AND user_id = ? AND kind = 'human' AND revoked_at IS NULL`,
		workspaceID, userID))
}

// Agents lists the active participants of a workspace.
func (s *Store) Agents(ctx context.Context, workspaceID int64) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE workspace_id = ? AND revoked_at IS NULL ORDER BY kind, name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// RevokeAgent disables an agent and frees its name. Its history stays readable.
func (s *Store) RevokeAgent(ctx context.Context, workspaceID, agentID int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE agents SET revoked_at = ?, token_hash = NULL
		WHERE id = ? AND workspace_id = ? AND kind = 'agent' AND revoked_at IS NULL`, nowMillis(), agentID, workspaceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RotateAgentToken replaces the token of an agent.
func (s *Store) RotateAgentToken(ctx context.Context, workspaceID, agentID int64, tokenHash string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE agents SET token_hash = ?
		WHERE id = ? AND workspace_id = ? AND kind = 'agent' AND revoked_at IS NULL`, tokenHash, agentID, workspaceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchAgent records that an agent was just active. It only writes when the old
// value is a few seconds stale, a disk sync on every tool call would be wasted.
func (s *Store) TouchAgent(ctx context.Context, agentID int64) error {
	now := nowMillis()
	_, err := s.db.ExecContext(ctx,
		`UPDATE agents SET last_seen_at = ? WHERE id = ? AND (last_seen_at IS NULL OR last_seen_at < ?)`,
		now, agentID, now-10_000)
	return err
}

const messageSelect = `
	SELECT m.id, m.workspace_id, m.thread_id, f.name, t.name, m.from_agent_id, m.to_agent_id,
	       m.kind, m.body, m.in_reply_to, m.created_at, m.delivered_at, m.read_at
	FROM messages m
	JOIN agents f ON f.id = m.from_agent_id
	JOIN agents t ON t.id = m.to_agent_id`

func scanMessage(row interface{ Scan(...any) error }) (*Message, error) {
	var m Message
	var reply, delivered, read sql.NullInt64
	if err := row.Scan(&m.ID, &m.WorkspaceID, &m.ThreadID, &m.From, &m.To, &m.FromID, &m.ToID,
		&m.Kind, &m.Body, &reply, &m.CreatedAt, &delivered, &read); err != nil {
		return nil, rowErr(err)
	}
	m.InReplyTo = nullMillis(reply)
	m.DeliveredAt = nullMillis(delivered)
	m.ReadAt = nullMillis(read)
	return &m, nil
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// InsertMessage stores a message. A threadID of zero starts a new thread that
// takes the id of the message itself.
func (s *Store) InsertMessage(ctx context.Context, workspaceID, threadID, fromID, toID int64, kind, body string, inReplyTo int64) (*Message, error) {
	m, _, err := s.InsertMessageKeyed(ctx, workspaceID, threadID, fromID, toID, kind, body, inReplyTo, "")
	return m, err
}

// MessageByClientKey finds a message the sender already stored under this key.
func (s *Store) MessageByClientKey(ctx context.Context, fromID int64, clientKey string) (*Message, error) {
	return scanMessage(s.db.QueryRowContext(ctx,
		messageSelect+` WHERE m.from_agent_id = ? AND m.client_key = ?`, fromID, clientKey))
}

// InsertMessageKeyed stores a message unless the sender already stored one under
// clientKey, in which case it returns that one and false.
func (s *Store) InsertMessageKeyed(ctx context.Context, workspaceID, threadID, fromID, toID int64, kind, body string, inReplyTo int64, clientKey string) (*Message, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var key any
	if clientKey != "" {
		key = clientKey
		existing, err := scanMessage(tx.QueryRowContext(ctx,
			messageSelect+` WHERE m.from_agent_id = ? AND m.client_key = ?`, fromID, clientKey))
		if err == nil {
			return existing, false, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, false, err
		}
	}
	var reply any
	if inReplyTo != 0 {
		reply = inReplyTo
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO messages (workspace_id, thread_id, from_agent_id, to_agent_id, kind, body, in_reply_to, created_at, client_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, workspaceID, threadID, fromID, toID, kind, body, reply, nowMillis(), key)
	if err != nil {
		return nil, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, false, err
	}
	if threadID == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET thread_id = id WHERE id = ?`, id); err != nil {
			return nil, false, err
		}
	}
	m, err := scanMessage(tx.QueryRowContext(ctx, messageSelect+` WHERE m.id = ?`, id))
	if err != nil {
		return nil, false, err
	}
	return m, true, tx.Commit()
}

// MessageByID finds a message in a workspace.
func (s *Store) MessageByID(ctx context.Context, workspaceID, id int64) (*Message, error) {
	return scanMessage(s.db.QueryRowContext(ctx, messageSelect+` WHERE m.id = ? AND m.workspace_id = ?`, id, workspaceID))
}

// ThreadExists reports whether a thread with that id exists in the workspace.
func (s *Store) ThreadExists(ctx context.Context, workspaceID, threadID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE workspace_id = ? AND thread_id = ? LIMIT 1`, workspaceID, threadID).Scan(&n)
	return n > 0, err
}

// ThreadLength counts the messages in a thread.
func (s *Store) ThreadLength(ctx context.Context, workspaceID, threadID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE workspace_id = ? AND thread_id = ?`, workspaceID, threadID).Scan(&n)
	return n, err
}

// RepliesTo returns the first reply to a message, or ErrNotFound if there is none yet.
func (s *Store) ReplyTo(ctx context.Context, messageID int64) (*Message, error) {
	return scanMessage(s.db.QueryRowContext(ctx,
		messageSelect+` WHERE m.in_reply_to = ? ORDER BY m.id LIMIT 1`, messageID))
}

// Unread lists messages the agent has not read yet, oldest first.
func (s *Store) Unread(ctx context.Context, agentID int64, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		messageSelect+` WHERE m.to_agent_id = ? AND m.read_at IS NULL ORDER BY m.id LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// Undelivered lists messages not yet pushed to the agent, oldest first.
func (s *Store) Undelivered(ctx context.Context, agentID int64, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, messageSelect+`
		WHERE m.to_agent_id = ? AND m.delivered_at IS NULL AND m.read_at IS NULL ORDER BY m.id LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// MarkDelivered records that messages were handed to their recipient's session.
func (s *Store) MarkDelivered(ctx context.Context, agentID int64, messageIDs ...int64) error {
	if len(messageIDs) == 0 {
		return nil
	}
	args := make([]any, 0, len(messageIDs)+2)
	args = append(args, nowMillis(), agentID)
	for _, id := range messageIDs {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE messages SET delivered_at = COALESCE(delivered_at, ?)
		WHERE to_agent_id = ? AND id IN (?`+strings.Repeat(",?", len(messageIDs)-1)+`)`, args...)
	return err
}

// MarkRead records that the recipient has seen the messages.
func (s *Store) MarkRead(ctx context.Context, agentID int64, messageIDs ...int64) error {
	if len(messageIDs) == 0 {
		return nil
	}
	args := make([]any, 0, len(messageIDs)+3)
	now := nowMillis()
	args = append(args, now, now, agentID)
	for _, id := range messageIDs {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE messages SET read_at = COALESCE(read_at, ?), delivered_at = COALESCE(delivered_at, ?)
		WHERE to_agent_id = ? AND id IN (?`+strings.Repeat(",?", len(messageIDs)-1)+`)`, args...)
	return err
}

// HistoryFilter narrows a history query. Zero values mean no restriction.
type HistoryFilter struct {
	ThreadID int64
	AgentID  int64 // either side of the conversation
	WithID   int64 // the other side, only used together with AgentID
	BeforeID int64
	Limit    int
}

// History returns matching messages, oldest first. The limit applies from the
// newest end so paging with BeforeID walks backwards through time.
func (s *Store) History(ctx context.Context, workspaceID int64, f HistoryFilter) ([]Message, error) {
	where := []string{`m.workspace_id = ?`}
	args := []any{workspaceID}
	if f.ThreadID != 0 {
		where = append(where, `m.thread_id = ?`)
		args = append(args, f.ThreadID)
	}
	if f.AgentID != 0 && f.WithID != 0 {
		where = append(where, `((m.from_agent_id = ? AND m.to_agent_id = ?) OR (m.from_agent_id = ? AND m.to_agent_id = ?))`)
		args = append(args, f.AgentID, f.WithID, f.WithID, f.AgentID)
	} else if f.AgentID != 0 {
		where = append(where, `(m.from_agent_id = ? OR m.to_agent_id = ?)`)
		args = append(args, f.AgentID, f.AgentID)
	}
	if f.BeforeID != 0 {
		where = append(where, `m.id < ?`)
		args = append(args, f.BeforeID)
	}
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx,
		messageSelect+` WHERE `+strings.Join(where, " AND ")+` ORDER BY m.id DESC LIMIT ?`, args...)
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

// Search runs a full text query over a workspace, best matches first. The query
// is quoted term by term so user input cannot use FTS operators or cause syntax errors.
func (s *Store) Search(ctx context.Context, workspaceID int64, query string, limit int) ([]Message, error) {
	match := ftsQuery(query)
	if match == "" {
		return []Message{}, nil
	}
	rows, err := s.db.QueryContext(ctx, messageSelect+`
		JOIN messages_fts ON messages_fts.rowid = m.id
		WHERE messages_fts MATCH ? AND m.workspace_id = ?
		ORDER BY messages_fts.rank LIMIT ?`, match, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

func ftsQuery(query string) string {
	terms := strings.Fields(query)
	for i, t := range terms {
		terms[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(terms, " ")
}

// Threads lists conversations in a workspace, most recently active first.
func (s *Store) Threads(ctx context.Context, workspaceID, beforeLastID int64, limit int) ([]Thread, error) {
	having := ""
	args := []any{workspaceID}
	if beforeLastID != 0 {
		having = `HAVING MAX(m.id) < ?`
		args = append(args, beforeLastID)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.thread_id, COUNT(*), MAX(m.created_at),
		       (SELECT r.body FROM messages r WHERE r.workspace_id = m.workspace_id AND r.thread_id = m.thread_id ORDER BY r.id LIMIT 1),
		       (SELECT GROUP_CONCAT(name, ',') FROM (
		            SELECT DISTINCT a.name AS name FROM messages x
		            JOIN agents a ON a.id = x.from_agent_id OR a.id = x.to_agent_id
		            WHERE x.thread_id = m.thread_id AND x.workspace_id = m.workspace_id))
		FROM messages m WHERE m.workspace_id = ?
		GROUP BY m.thread_id `+having+` ORDER BY MAX(m.id) DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Thread{}
	for rows.Next() {
		var t Thread
		var preview, names sql.NullString
		if err := rows.Scan(&t.ID, &t.Messages, &t.LastAt, &preview, &names); err != nil {
			return nil, err
		}
		t.Preview = preview.String
		if len(t.Preview) > 200 {
			t.Preview = strings.ToValidUTF8(t.Preview[:200], "")
		}
		t.Participants = strings.Split(names.String, ",")
		out = append(out, t)
	}
	return out, rows.Err()
}

// PruneMessages deletes messages older than the cutoff and returns how many went.
// It works in small batches, so senders are not locked out during a big cleanup.
func (s *Store) PruneMessages(ctx context.Context, olderThan int64) (int64, error) {
	var total int64
	for {
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM messages WHERE id IN (SELECT id FROM messages WHERE created_at < ? LIMIT 1000)`, olderThan)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n == 0 {
			return total, nil
		}
	}
}

// UnansweredAsks lists recent questions to an agent that never got a reply.
func (s *Store) UnansweredAsks(ctx context.Context, agentID int64, limit int) ([]Message, error) {
	since := nowMillis() - 7*24*60*60*1000
	rows, err := s.db.QueryContext(ctx, messageSelect+`
		WHERE m.to_agent_id = ? AND m.kind = 'ask' AND m.created_at > ?
		AND NOT EXISTS (SELECT 1 FROM messages r WHERE r.in_reply_to = m.id)
		ORDER BY m.id LIMIT ?`, agentID, since, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}
