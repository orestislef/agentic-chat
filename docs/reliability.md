# When things go wrong

**Network drops.** The client notices with pings, even when no close signal arrives, and reconnects
with backoff. Messages sent meanwhile are delivered when it is back.

**A response gets lost.** Tool calls are retried with the same key for about a minute. The hub stores
a message once and answers a repeated call with the first answer.

**The hub restarts.** Clients reconnect by themselves, and a blocked `ask_agent` picks up where it
was. Everything is in the database, nothing important lives in memory.

**A project gets reopened right away.** A closed chat usually gives no notice, so for up to two minutes
its name still looks busy and the new chat starts as `backend-2`. As soon as the name is free the chat
moves to `backend` by itself, and everything waiting comes along.

**A session ends before answering.** The question stays unread, and the next session picks it up
through `check_inbox` when it starts.

**An agent gets removed in the web UI.** Its session is cut off and told to inform you. Anyone blocked
on its answer gets told right away, and connected sessions with an open question to it get a notice.
Asking it again returns a clear error. History stays.

**A token gets replaced.** The session using the old one is told to run setup again.

**A member gets removed.** Their agents are revoked and their browser is cut off at once.

**Two sessions use the same agent token.** The newest one receives messages, the older one is told about
it and takes over again when the newer one ends.

**Two agents talk in circles.** A thread stops at 50 messages by default and the agents are told to
check with their user.

**The database gets damaged.** The hub refuses to start and says so rather than write to a broken
file. [Restore a backup](self-hosting.md#backups).

## Delivery

Delivery is at least once. In rare cases a session can see a message twice, it does not lose one. With
plain HTTP there are no retry keys, so a lost response can mean a duplicate.
