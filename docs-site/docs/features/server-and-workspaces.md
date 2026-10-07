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

### Sub-agent sessions

:::info[Fork feature]
The sub-agent tree and the `children` endpoint are additions in the
`joestump-agent/crush` fork.
:::

Every sub-agent run — `agent`, `agentic_fetch`, and
[dispatched agents](/agents/overview) — gets a task session that is a child of
the session that started it. The picker nests them under their parent (see
[Sessions](/features/sessions#sub-agent-sessions)), and a client lists them
with:

```text
GET /v1/workspaces/{id}/sessions/{sid}/children
```

It returns the child sessions of `{sid}`, oldest update first. The server
describes the rest of its HTTP API at `/v1/docs/`, with the OpenAPI spec at
`/v1/docs/openapi.json`.

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

## Dispatched agents against a server

:::info[Fork feature]
[Multi-agent dispatch](/agents/overview) is an addition in the
`joestump-agent/crush` fork.
:::

Dispatch is built for the in-process TUI. A client talking to a server —
`crush --host …`, or `CRUSH_CLIENT_SERVER=1` — reads none of the live dispatch
state, so:

- an agent block shows the state its tool call recorded at dispatch and never
  updates live — no status changes, todo line, or elapsed time;
- the `@` completions list no agents;
- a prompt that starts with `@handle` is **not** routed to the agent — it goes
  to the main agent as an ordinary prompt;
- a mid-sentence `@handle` attaches no agent card.

Transcripts still come from the session store, so the
[sub-agent session tree](#sub-agent-sessions) and inspect mode can open a
sub-agent's transcript. Making the TUI a real A2A client of the server is
tracked as [#421](https://github.com/joestump-agent/crush/issues/421).

The server already proxies each workspace's
[A2A host](/agents/a2a-protocol) for that client:

```
GET  /v1/workspaces/{id}/a2a/agents
POST /v1/workspaces/{id}/a2a/agents/{dispatch id}
```

- **The agent index.** The `GET` is the host's
  [agent index](/agents/a2a-protocol#agent-index): a JSON snapshot, or, with
  `Accept: text/event-stream`, the snapshot followed by one upsert per change. A
  stream opened before anything is dispatched waits for the host to start. A
  snapshot answers `503` until it has.
- **A dispatched agent.** The `POST` carries one A2A JSON-RPC request to a
  dispatched agent: a steer, a cancel, or a task read. It streams the answer
  when the method streams.
- **Credentials.** The server authenticates to the host with the host's own
  token, which never leaves the server process. Every other check the host
  makes still applies to the request as the client sent it, so an `Origin`
  header is refused.

The TUI follows this surface in place of the dispatch registry once the UI moves
onto it (#421, step 3); until then, the list above still holds.

:::warning[Known issue]
Even the server-side half is unreliable today. The server builds the agent
coordinator on the client's init request context, which ends as soon as that
request returns, so dispatch progress stops being tracked
(tracked as [#419](https://github.com/joestump-agent/crush/issues/419)). And every client attach replaces the coordinator,
orphaning dispatches that are already running (tracked as [#420](https://github.com/joestump-agent/crush/issues/420)). Use
dispatch from a plain in-process `crush` for now.
:::
