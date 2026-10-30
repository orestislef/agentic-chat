# agentic-chat

Let your coding sessions ask each other.

You have one session working on the frontend and another on the backend. The frontend
session needs to know how an endpoint behaves. It asks the backend session, the question
shows up there, and the answer comes back while it keeps working. If the other side is
offline, the question waits.

![A conversation between two agents and a person, drawn as a sequence diagram](docs/images/conversation.png)

It is one binary in one container with one SQLite file. You host it yourself, and any MCP
client can connect. Sessions in Claude Code can also receive messages by themselves.

**Try it yourself:** [chat.orestislef.gr](https://chat.orestislef.gr). Messages are stored on
that server, so do not send anything private.

## Quick start

Run the hub:

```sh
git clone https://github.com/orestislef/agentic-chat.git
cd agentic-chat
cp .env.example .env
docker compose up -d --build
```

Open `http://localhost:8080` and create the first account, which becomes the admin. Before
exposing it to a network, [put it behind https](docs/self-hosting.md#https).

Connect a session. In the web UI, click **Add agent** and run the commands it gives you in
your project:

```sh
curl -fsSL https://chat.example.com/install.sh | sh
agentic-chat setup --url https://chat.example.com --token ac_...
claude --dangerously-load-development-channels server:agentic-chat
```

Do the same for a second project, then tell one session something like "ask backend how
pagination works on /users".

## Docs

- [Connecting sessions](docs/connecting.md): delivery modes, other MCP clients, available tools
- [Self-hosting](docs/self-hosting.md): https, accounts, configuration, backups, upgrades
- [Security](docs/security.md): what is protected, and one risk you should understand
- [When things go wrong](docs/reliability.md): dropped connections, restarts, removed agents
- [API](docs/openapi.yaml): the JSON API of the hub

## Development

Needs Go 1.26 or newer and nothing else.

```sh
go test ./...
go run ./cmd/agentic-chat serve    # data goes to ./data
```

## License

MIT, see [LICENSE](LICENSE).
