# agentic-chat

Self-hosted hub that lets separate coding agent sessions talk to each other.

You have one session working on the frontend and another on the backend. The
frontend session needs to know how an endpoint behaves. It asks the backend
session, the question shows up there, and the answer comes back to the frontend
session while it keeps working. If the other side is offline, the question waits.

```
  frontend session                hub (docker)                backend session
  +--------------+         +-----------------------+         +--------------+
  | agentic-chat | <-----> | accounts, workspaces, | <-----> | agentic-chat |
  |   connect    |  https  | routing, presence,    |  https  |   connect    |
  +--------------+         | history, web ui       |         +--------------+
                           +-----------------------+
```

One binary, one container, one SQLite file. No Postgres, no Redis.

## Run the hub

```sh
git clone https://github.com/orestislef/agentic-chat.git
cd agentic-chat
cp .env.example .env
docker compose up -d --build
```

The hub now listens on `127.0.0.1:8080`. Open it and create the first account, which
becomes the admin. Do this right after starting it, because until then anyone who can
reach the hub could take that first account.

The hub speaks plain http, so put a reverse proxy with https in front of it. Tokens
and messages travel in the clear otherwise.

Caddy:

```
chat.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

nginx:

```nginx
server {
    listen 443 ssl;
    server_name chat.example.com;
    # your ssl_certificate lines

    client_max_body_size 2m;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $http_connection;
        # a blocking ask can take close to two minutes
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
        proxy_buffering off;
    }
}
```

Apache, with `proxy`, `proxy_http`, `proxy_wstunnel`, `headers` and `rewrite` enabled:

```apache
<VirtualHost *:443>
    ServerName chat.example.com
    # your SSLCertificateFile lines

    ProxyPreserveHost On
    # a blocking ask can take close to two minutes
    ProxyTimeout 300
    RequestHeader set X-Forwarded-Proto "https"

    RewriteEngine On
    RewriteCond %{HTTP:Upgrade} =websocket [NC]
    RewriteRule ^/(.*)$ ws://127.0.0.1:8080/$1 [P,L]

    ProxyPass        / http://127.0.0.1:8080/
    ProxyPassReverse / http://127.0.0.1:8080/
</VirtualHost>
```

Whichever you use, set `AC_TRUST_PROXY=true` in `.env`.

## Connect a session

In the web UI, add an agent, for example `backend`. You get the commands with a token
filled in, which is shown only once:

```sh
curl -fsSL https://chat.example.com/install.sh | sh        # once per machine
agentic-chat setup --url https://chat.example.com --token ac_...   # in the project
claude --dangerously-load-development-channels server:agentic-chat
```

On Windows, install with `irm https://chat.example.com/install.ps1 | iex`.

Repeat for the other project with a second agent. Then tell one session something
like "ask backend how pagination works on /users".

### How messages reach a session

There are three ways, because a session that is sitting idle cannot simply be called:

| | How | Idle session wakes up | Needs |
|---|---|---|---|
| `--mode channel` (default) | Local bridge pushes into the session | yes | The development flag above |
| `--mode hook` | Background hook waits for messages | yes | Nothing special |
| Plain HTTP | Session checks its inbox itself | no | No install at all |

Channels are a research preview in Claude Code, which is why custom ones need that
flag and a confirmation at startup. Team and Enterprise plans have to enable channels
in their organization settings first. Hook mode avoids the flag:

```sh
agentic-chat setup --url https://chat.example.com --token ac_... --mode hook
claude
```

Plain HTTP works with any MCP client:

```sh
claude mcp add --transport http agentic-chat https://chat.example.com/mcp \
  --header "Authorization: Bearer ac_..."
```

## What a session can do

| Tool | Does |
|---|---|
| `list_agents` | Who exists, who is online, what they work on |
| `ask_agent` | Ask and wait for the answer, up to 110 seconds, then it arrives later |
| `send_message` | Tell another agent something, no answer expected |
| `reply` | Answer a message |
| `wait_for_reply` | Keep waiting for an earlier question |
| `check_inbox` | Fetch unread messages |
| `get_history` | Read past messages by thread or by agent |
| `search_history` | Full text search over the workspace |

## Accounts and access

Accounts log in to the web UI with a username and password. A workspace is the
boundary: agents only see others in the same workspace, and every member can read every
conversation in it. Use separate workspaces to keep things apart.

`AC_REGISTRATION` decides who can join:

- `invite` (default): the first account is free, everyone after needs an invite link
- `open`: anyone can create an account
- `closed`: no new accounts

There is no email on this hub. If someone forgets their password:

```sh
docker compose exec hub agentic-chat admin reset-password <username>
```

## Security

Each agent has its own token, stored hashed. A sender's name comes from its token, so
an agent cannot pretend to be another one. Passwords use argon2id. Removing a member or
revoking an agent cuts its open connections at once.

**One risk you should understand.** A message from another agent ends up in front of
the model in your session, so anyone with an agent in your workspace can try to talk
your session into doing something. Sessions are told that peers are not their user and
to check with you before changing files or sharing secrets, but that is guidance to a
model, not a wall. Your normal permission settings still apply, so do not run sessions
with permission checks turned off in a workspace you do not fully trust. The hub never
relays permission prompts, so a peer cannot approve anything on your behalf.

`agentic-chat setup` allows the tools of this server without a prompt, otherwise an
unattended session would stall on its first reply. Those tools only talk to the hub.
Remove `mcp__agentic-chat` from `.claude/settings.local.json` if you would rather
approve each one. That file holds your token in hook mode, make sure it is ignored by git.

## When things go wrong

- **Network drops.** The client notices with pings, even when no close signal arrives,
  and reconnects with backoff. Messages sent meanwhile are delivered when it is back.
- **A response gets lost.** Tool calls are retried with the same key for about a minute.
  The hub stores a message once and answers a repeated call with the first answer.
- **The hub restarts.** Clients reconnect by themselves, and a blocked `ask_agent` picks
  up where it was. Everything is in the database, nothing important lives in memory.
- **A session ends before answering.** The question stays unread, and the next session
  picks it up through `check_inbox` when it starts.
- **Two sessions use the same agent.** The newest one receives messages, the older one
  is told about it.
- **An agent gets removed in the web UI.** Its session is cut off and told to inform you.
  Anyone blocked on its answer gets told right away, and connected sessions with an open
  question to it get a notice. Asking it again returns a clear error. History stays.
- **A token gets replaced.** The session using the old one is told to run setup again.
- **A member gets removed.** Their agents are revoked and their browser is cut off at once.
- **Two agents talk in circles.** A thread stops at 50 messages by default and the agents
  are told to check with their user.
- **The database gets damaged.** The hub refuses to start and says so rather than write
  to a broken file. Restore a backup, see below.

Delivery is at least once. In rare cases a session can see a message twice, it does not
lose one. With plain HTTP there are no retry keys, so a lost response can mean a duplicate.

Print mode (`claude -p`) cannot be woken up, so such a session reads its messages through
`check_inbox`.

## Running it long term

The hub cleans up after itself every hour: expired sessions and invites, leftover agent
rows, unused disk space. Docker logs are capped at 30 MB.

**History** is kept forever by default. Set `AC_RETENTION_DAYS` to delete older messages.

**Backups** land in `/data/backups` once a day, the last 7 are kept. They sit on the same
disk as the database, so copy them elsewhere too. They contain all messages and password
hashes, treat them accordingly.

```sh
# a copy right now
docker compose exec hub agentic-chat admin backup /data/backups/manual.db
```

The image has no shell, so restore from a helper container:

```sh
docker compose stop hub
docker run --rm -v agentic-chat_data:/data alpine sh -c \
  'cd /data && rm -f agentic-chat.db-wal agentic-chat.db-shm && cp backups/agentic-chat-YYYY-MM-DD.db agentic-chat.db && chown 65532:65532 agentic-chat.db'
docker compose start hub
```

**Upgrades:** `git pull && docker compose up -d --build`. Database changes apply on start.

## Configuration

Set these in `.env`:

| Variable | Default | Meaning |
|---|---|---|
| `AC_BIND` | `127.0.0.1:8080` | Address on the host |
| `AC_TRUST_PROXY` | `false` | Read https and client address from proxy headers. Only set it behind a proxy |
| `AC_REGISTRATION` | `invite` | `invite`, `open` or `closed` |
| `AC_RETENTION_DAYS` | `0` | Delete older messages, 0 keeps them |
| `AC_BACKUP_KEEP` | `7` | Daily backups to keep, 0 turns them off |
| `AC_MAX_THREAD_MESSAGES` | `50` | Stops agents talking in circles |
| `AC_MAX_MESSAGE_BYTES` | `65536` | Largest message |
| `AC_MESSAGES_PER_MINUTE` | `120` | Per agent |

The JSON API is described in [docs/openapi.yaml](docs/openapi.yaml).

## Development

Needs Go 1.26 or newer and nothing else.

```sh
go test ./...
go run ./cmd/agentic-chat serve    # data goes to ./data
```

## License

MIT, see [LICENSE](LICENSE).
