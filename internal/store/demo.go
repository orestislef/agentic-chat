package store

import "context"

// IsDemoWorkspace reports whether a workspace was created under demo mode.
func (s *Store) IsDemoWorkspace(ctx context.Context, workspaceID int64) (bool, error) {
	var demo bool
	err := s.db.QueryRowContext(ctx, `SELECT demo FROM workspaces WHERE id = ?`, workspaceID).Scan(&demo)
	return demo, rowErr(err)
}

// CountCodingAgents counts the active coding agents of a workspace.
func (s *Store) CountCodingAgents(ctx context.Context, workspaceID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agents WHERE workspace_id = ? AND kind = 'agent' AND revoked_at IS NULL`, workspaceID).Scan(&n)
	return n, err
}

// CountMessagesSince counts the messages of a workspace newer than a moment.
func (s *Store) CountMessagesSince(ctx context.Context, workspaceID, since int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE workspace_id = ? AND created_at >= ?`, workspaceID, since).Scan(&n)
	return n, err
}

// CountDemoWorkspaces counts the demo workspaces a user belongs to.
func (s *Store) CountDemoWorkspaces(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM memberships m JOIN workspaces w ON w.id = m.workspace_id
		WHERE m.user_id = ? AND w.demo = 1`, userID).Scan(&n)
	return n, err
}

// PruneDemo deletes demo workspaces older than the cutoff with everything in them,
// then demo accounts left with nothing. Rows without the demo mark are never touched.
func (s *Store) PruneDemo(ctx context.Context, olderThan int64) (workspaces, users int64, err error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM workspaces WHERE demo = 1 AND created_at < ?`, olderThan)
	if err != nil {
		return 0, 0, err
	}
	if workspaces, err = res.RowsAffected(); err != nil {
		return 0, 0, err
	}
	res, err = s.db.ExecContext(ctx, `
		DELETE FROM users WHERE demo = 1 AND created_at < ?
		AND NOT EXISTS (SELECT 1 FROM memberships WHERE user_id = users.id)
		AND NOT EXISTS (SELECT 1 FROM agents WHERE user_id = users.id)`, olderThan)
	if err != nil {
		return workspaces, 0, err
	}
	users, err = res.RowsAffected()
	return workspaces, users, err
}
