---
id: server-and-workspaces
title: Server and workspaces
sidebar_position: 11
description: Run crush server, share a workspace across clients, and understand the lifetime rules.
---

# Server and workspaces

Crush normally runs the agent in-process. It can also run as a **server** that
several clients talk to, which is what lets two TUIs — or a TUI and something
else — share one live session.

## Starting a server

```bash
crush server
crush server --host unix:///tmp/crush.sock
crush server --host tcp://127.0.0.1:8099
```

With no `--host`, Crush picks a per-user default: a Unix socket named
`crush-<uid>.sock` in the socket directory, or a named pipe
(`npipe:////./pipe/crush-<uid>.sock`) on Windows.

Point a client at it with the same flag:

```bash
crush --host unix:///tmp/crush.sock
```

`--host` / `-H` is a persistent flag, so it works on every subcommand.

Server logs go to a per-host file under the cache directory, separate from the
project logs. Run `crush dirs` to see where.

## Workspaces

Clients are grouped into **workspaces** keyed by their resolved `--cwd`. Two
clients with the same `--cwd` join the same underlying workspace, and therefore
share:

- the session list
- message history
- the permission queue
- LSP state
- MCP state

Joining is implicit — pointing a second client at the same working directory
attaches it to the existing workspace.

## Sessions inside a shared workspace

Each new invocation starts in its **own fresh session** by default. To pick up
the conversation another client already has open, use the session picker
(<kbd>ctrl+s</kbd>) and select it.

Two signals in the picker tell you what is going on:

| Signal | Meaning |
| --- | --- |
| `IsBusy` | An agent turn is in flight for that session |
| `AttachedClients` | How many clients are currently viewing it |

A non-zero `AttachedClients` — often together with `IsBusy` — is the cue that a
session is in progress on another client, and that joining it will mirror that
view live.

## First-wins flags

The first client to create a workspace fixes its process-wide flags. In
particular **`--yolo` and `--debug` follow a first-wins rule**: a later client
arriving at the same `--cwd` with different values does *not* change the running
workspace. A debug log line records the mismatch, and the workspace keeps the
flags it was created with.

This matters: joining a workspace someone else created with `--yolo` means you
are in yolo mode whether you asked for it or not.

## Lifetime

A workspace lives as long as at least one client has an SSE event stream open
against it. When the last stream disconnects, the workspace is torn down — but
with two grace windows that exist to stop ordinary network hiccups from
destroying state:

| Window | Default | Purpose | Override |
| --- | --- | --- | --- |
| Create grace | 30s | A client that has created a workspace but not yet opened its event stream isn't reaped before it can attach | — |
| Detach grace | 10s | A client's claim survives a stream dropping without an explicit release, so a reconnect finds the same workspace ID. A clean exit releases first and skips the grace. | `CRUSH_SERVER_DETACH_GRACE` (seconds; `0` = immediate teardown) |
| Idle shutdown | 60s | The server stays alive after its last workspace is released, so a client closing one session and opening another reuses the running server instead of racing its shutdown. Any workspace create in the window cancels the pending shutdown. | `CRUSH_SERVER_IDLE_TIMEOUT` (seconds; `0` = shut down immediately) |

`CRUSH_SERVER_READY_TIMEOUT` (a Go duration) bounds how long a client waits for
a server to become ready.

## Channels against a server

[Channel](/features/channels) delivery works against a shared backend. The
server routes each event **exactly once**: into the session an attached client
is viewing — the most recently updated one when clients are viewing different
sessions — otherwise into the workspace's most recent session, creating one only
when none exists.

That holds even with no clients connected, so a headless server still processes
channel pushes. Attached clients see the injected turn arrive through the normal
event stream.
