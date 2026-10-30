package store

// migrations run in order, each exactly once. Never edit an applied entry, append a new one.
var migrations = []string{`
CREATE TABLE users (
	id            INTEGER PRIMARY KEY,
	username      TEXT NOT NULL COLLATE NOCASE UNIQUE,
	password_hash TEXT NOT NULL,
	is_admin      INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL
);

CREATE TABLE sessions (
	token_hash TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE workspaces (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE memberships (
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	role         TEXT NOT NULL CHECK (role IN ('owner', 'member')),
	created_at   INTEGER NOT NULL,
	PRIMARY KEY (workspace_id, user_id)
);
CREATE INDEX memberships_user ON memberships(user_id);

CREATE TABLE invites (
	id           INTEGER PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	code_hash    TEXT NOT NULL UNIQUE,
	created_by   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	max_uses     INTEGER NOT NULL,
	uses         INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL
);
CREATE INDEX invites_workspace ON invites(workspace_id);

-- Every participant is an agent row. People get kind 'human' and no token,
-- so messages always go from one agent row to another.
CREATE TABLE agents (
	id           INTEGER PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	description  TEXT NOT NULL DEFAULT '',
	kind         TEXT NOT NULL CHECK (kind IN ('agent', 'human')),
	token_hash   TEXT UNIQUE,
	last_seen_at INTEGER,
	created_at   INTEGER NOT NULL,
	revoked_at   INTEGER
);
CREATE UNIQUE INDEX agents_active_name ON agents(workspace_id, name) WHERE revoked_at IS NULL;

CREATE TABLE messages (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	workspace_id  INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	thread_id     INTEGER NOT NULL,
	from_agent_id INTEGER NOT NULL REFERENCES agents(id),
	to_agent_id   INTEGER NOT NULL REFERENCES agents(id),
	kind          TEXT NOT NULL CHECK (kind IN ('ask', 'reply', 'note')),
	body          TEXT NOT NULL,
	in_reply_to   INTEGER,
	created_at    INTEGER NOT NULL,
	delivered_at  INTEGER,
	read_at       INTEGER
);
CREATE INDEX messages_inbox ON messages(to_agent_id, read_at, id);
CREATE INDEX messages_thread ON messages(workspace_id, thread_id, id);
CREATE INDEX messages_reply ON messages(in_reply_to);
CREATE INDEX messages_created ON messages(created_at);

CREATE VIRTUAL TABLE messages_fts USING fts5(body, content='messages', content_rowid='id');
CREATE TRIGGER messages_fts_insert AFTER INSERT ON messages BEGIN
	INSERT INTO messages_fts(rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER messages_fts_delete AFTER DELETE ON messages BEGIN
	INSERT INTO messages_fts(messages_fts, rowid, body) VALUES ('delete', old.id, old.body);
END;
`, `
-- Lets a client retry a send after a lost response without creating a duplicate.
ALTER TABLE messages ADD COLUMN client_key TEXT;
CREATE UNIQUE INDEX messages_client_key ON messages(from_agent_id, client_key) WHERE client_key IS NOT NULL;
`, `
-- Set on what gets created while demo mode is on. Only these rows are ever limited or
-- deleted by it, so turning it on cannot touch anything that existed before.
ALTER TABLE users ADD COLUMN demo INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workspaces ADD COLUMN demo INTEGER NOT NULL DEFAULT 0;
`, `
-- Newest first within a workspace, for history and the list of conversations.
CREATE INDEX messages_workspace ON messages(workspace_id, id);
-- Only what is still open needs indexing, read messages are the vast majority and stay out.
DROP INDEX messages_inbox;
CREATE INDEX messages_unread ON messages(to_agent_id, id) WHERE read_at IS NULL;
CREATE INDEX messages_undelivered ON messages(to_agent_id, id) WHERE delivered_at IS NULL AND read_at IS NULL;
`, `
-- Browser sign in for MCP clients, and one agent per chat on top of it.
CREATE TABLE oauth_clients (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL,
	redirect_uris TEXT NOT NULL,
	created_at    INTEGER NOT NULL
);
CREATE TABLE oauth_codes (
	code_hash    TEXT PRIMARY KEY,
	client_id    TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	redirect_uri TEXT NOT NULL,
	challenge    TEXT NOT NULL,
	expires_at   INTEGER NOT NULL
);
CREATE TABLE connections (
	id           INTEGER PRIMARY KEY,
	token_hash   TEXT NOT NULL UNIQUE,
	client_name  TEXT NOT NULL,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL
);
CREATE INDEX connections_user ON connections(user_id);
CREATE TABLE chat_sessions (
	session_hash  TEXT PRIMARY KEY,
	connection_id INTEGER NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
	agent_id      INTEGER REFERENCES agents(id) ON DELETE SET NULL,
	last_seen_at  INTEGER NOT NULL
);
CREATE INDEX chat_sessions_agent ON chat_sessions(agent_id);
-- Agents a chat created for itself. These get cleaned up after a long idle time.
ALTER TABLE agents ADD COLUMN auto INTEGER NOT NULL DEFAULT 0;
`, `
-- The name a chat asked for, which can differ from what it got while that name was busy.
ALTER TABLE chat_sessions ADD COLUMN wanted_name TEXT NOT NULL DEFAULT '';
`}
