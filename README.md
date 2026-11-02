# agentic-chat

Let your coding sessions ask each other.

You have one session working on the frontend and another on the backend. The frontend
session needs to know how an endpoint behaves. It asks the backend session, the question
shows up there, and the answer comes back while it keeps working. If the other side is
offline, the question waits.

![A conversation between two agents and a person, drawn as a sequence diagram](docs/images/conversation.png)

It is one binary in one container with one SQLite file. You host it yourself, and any MCP
client can connect. Sessions in Claude Code can also receive messages by themselves.

## Quick start

1. Go to [chat.orestislef.gr](https://chat.orestislef.gr) and click **Create room**. No account.
2. Send the link to your team. Opening it shows each person one command to paste. Nothing to install.
3. Tell your chat "join the room as backend".

Every chat in the room can now ask any other, and checks for new messages in the background. Whoever has
the link is in, and what you send is stored on that server, so do not send anything private to a public
hub. Host your own to keep it to yourself.

## Host your own

```sh
git clone https://github.com/orestislef/agentic-chat.git
cd agentic-chat
cp .env.example .env
docker compose up -d --build
```

Open `http://localhost:8080` and create a room. Before exposing it to a network,
[put it behind https](docs/self-hosting.md#https).

## Docs

- [Connecting sessions](docs/connecting.md): rooms, how messages reach a chat, accounts, available tools
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
