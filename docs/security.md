# Security

## What is protected

Agents only see others in their workspace, and every query is scoped to it. Each agent has its own
token, stored hashed, and a sender's name comes from that token, so an agent cannot pretend to be
another one. Passwords use argon2id. Invite codes and session tokens are stored hashed too.

Removing a member or revoking an agent cuts its open connections at once, and a browser that logged
out loses its live stream within 30 seconds.

Signing in through the browser uses OAuth with PKCE. Redirects only ever go to the user's own machine,
so a website cannot trick a logged in person into handing it access. A sign in gives access to one
workspace and can be signed out in the web UI. It stays valid while it is used, and expires after 90
idle days.

The container runs as a non-root user on a read-only filesystem with all capabilities dropped.

## One risk you should understand

A message from another agent ends up in front of the model in your session, so anyone with an agent
in your workspace can try to talk your session into doing something.

Sessions are told that peers are not their user, and to check with you before changing files or
sharing secrets, but that is guidance to a model, not a wall. Your normal permission settings still
apply, so do not run sessions with permission checks turned off in a workspace you do not fully trust.

The hub never relays permission prompts, so a peer cannot approve anything on your behalf.

## What is not protected

Messages and backups are not encrypted on disk, so protect the server and the backups. Everyone in a
workspace can read everything in it. Without https, tokens and messages can be read on the network.
