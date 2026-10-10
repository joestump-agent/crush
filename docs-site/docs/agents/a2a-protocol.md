---
id: a2a-protocol
title: A2A wire reference
sidebar_label: A2A protocol
description: What the per-process A2A host exposes — the Agent Card, endpoints, the JSON-RPC methods Crush calls, the event stream and its todo metadata, the SQLite task store, the security model, the TCP listener and external agents.
---

# A2A wire reference

:::info[Fork feature]
The A2A layer is an addition in the `joestump-agent/crush` fork.
:::

Every dispatched agent is served over the
[A2A protocol](https://a2a-protocol.org) while it runs, and every agent
definition has a route on the same host. This page documents exactly what is
on the wire, so you can debug it, read logs against it, or build against it.
How the host fits into a dispatch is on [Architecture](./architecture.md);
what a team can do with it is on [Working with other agents](./team.md).

:::info[Who can call it]
Crush's own coordinator is the only client that can drive a run. The
opt-in [TCP listener](#tcp-listener) requires TLS and authenticates every
call, but it exposes no local dispatch or sub-agent run yet. A dispatch's
route exists only while that dispatch runs; a definition's route is permanent
and rejects every task until an entry point serves a turn on it.
:::

## At a glance

| Item | Today |
| --- | --- |
| SDK | [`github.com/a2aproject/a2a-go/v2`](https://github.com/a2aproject/a2a-go) v2.5.0 |
| Protocol version | `1.0`, stamped on the card's interface |
| Binding | JSON-RPC 2.0 over HTTP; streaming responses are Server-Sent Events |
| Listener | One host per process on a unix socket: `<data dir>/a2a/<pid>.sock`, socket mode `0600` in a `0700` directory the host verifies before binding, or `$TMPDIR/crush-a2a-<uid>/<pid>-<hash>.sock` when that path would exceed the 104-byte socket limit. Optionally also TLS on TCP; see [TCP listener](#tcp-listener) |
| Lifetime | The host starts with the process and serves every definition's route for its lifetime; a dispatch's route lasts from just after provisioning until the run returns, with a 5-second graceful shutdown |
| Discovery | In memory: a dispatch's endpoint and card are stamped on its registry entry, and the definition cards are kept in the host's listing. Every route also serves its card at `GET /agents/<id>/.well-known/agent-card.json`; see [Fetching a card](#fetching-a-card) |
| Task store | SQLite: the `a2a_tasks` table of the session database, shared by every route, so tasks survive a restart. Each dispatch also leaves a durable record in `a2a_dispatches`, reconciled at startup |
| Authentication | The host's per-process bearer token; on the TCP listener, a verified client certificate when `client_ca` is set |

Method names below are the A2A 1.0 names the SDK uses. Older A2A material
calls `SendStreamingMessage` `message/stream`, and `CancelTask`
`tasks/cancel`.

## Agent Card

The coordinator builds a card for each dispatch and stamps it on the
registry entry, and one for each enabled agent definition, kept in the
host's listing. Every route serves its card at
`GET /agents/<id>/.well-known/agent-card.json`
([#580](https://github.com/joestump-agent/crush/issues/580)); see
[Fetching a card](#fetching-a-card). The coordinator itself never fetches
one — it reads the same object from the registry entry. A dispatch's card:

```json
{
  "name": "tester",
  "description": "tester",
  "version": "devel",
  "supportedInterfaces": [
    {
      "url": "http://crush-a2a/agents/dispatch-1",
      "protocolBinding": "JSONRPC",
      "protocolVersion": "1.0"
    }
  ],
  "capabilities": { "streaming": true },
  "defaultInputModes": ["text/plain"],
  "defaultOutputModes": ["text/plain"],
  "skills": [
    {
      "id": "crush-hooks",
      "name": "crush-hooks",
      "description": "Write and debug Crush hooks.",
      "tags": ["crush-skill"]
    }
  ]
}
```

| Field | Source |
| --- | --- |
| `name` | The dispatch's assigned `@handle`, without the `@`. A definition card carries the definition's `name` (`Coder`, `Worker`, …). |
| `description` | The dispatch's `role`, which may be empty. A definition card carries the definition's `description`. |
| `version` | The Crush build version. |
| `supportedInterfaces[0]` | The routed endpoint, `http://crush-a2a/agents/<dispatch id>`, JSON-RPC binding, protocol `1.0`. Always first. |
| `supportedInterfaces[1]` | Only while the [TCP listener](#tcp-listener) runs and has an address to advertise: `https://<host:port>/agents/<dispatch id>`, same binding and protocol. |
| `capabilities` | `streaming: true` and the declared `extensions`: `todos/v1`, `usage/v1`, `questions/v1`, `answers/v1`, `permissions/v1`, `permission-decisions/v1`, `undelivered-steers/v1` and `steer-refusals/v1`, each with its JSON Schema in its params. No push notifications, no extended card. |
| `defaultInputModes`, `defaultOutputModes` | `text/plain` both ways. |
| `skills` | One entry per Crush skill the dispatch was given: every discovered skill when the dispatch named none. `id` and `name` are the skill name, and every entry carries the single tag `crush-skill`. Empty on a definition card. |

The card declares the `crush-bearer` security scheme (see
[Security model](#security-model)) and, when the TCP listener requires
client certificates, a `crush-mtls` scheme as the alternative. It declares
no `provider`.

### Fetching a card

Each route — a running dispatch's, or an agent definition's — serves its
card at the A2A well-known path under the route:

```bash
# On the socket: a bare GET, as any A2A client sends it. No token, no
# Content-Type; the Host must be the crush-a2a label.
curl --unix-socket "$DATA_DIR/a2a/$PID.sock" \
  http://crush-a2a/agents/coder/.well-known/agent-card.json

# Over the TCP listener: the gate authenticates first, with the client
# certificate (shown) or the host's bearer token.
curl --cacert ca.pem --cert bob.pem --key bob-key.pem \
  https://alice.example.internal:7443/agents/coder/.well-known/agent-card.json
```

What comes back depends on the listener the request arrived on:

- **On the socket**, the card as built: the object the registry entry and
  the [agent index](#agent-index) carry, with the socket interface first
  and the HTTPS interface second while the TCP listener runs.
- **Over TCP**, a copy with one interface: the route at the origin the
  caller dialed, `https://<Host>/agents/<id>`. The socket label is dropped,
  because no remote client can dial it and an SDK client takes the first
  interface it supports. The caller's own origin stands in for the
  advertised address, so a card read through another certificate name —
  or from a listener with no address to advertise — still names a URL its
  reader can reach; Crush's own [external-agent](#external-agents)
  resolver refuses a card whose JSON-RPC service is on another origin than
  the card's. Everything else on the card is unchanged.

The card is discovery metadata, which the A2A spec has clients fetch with a
bare, uncredentialed `GET`, so the card path checks no credential of its
own. On the socket, the `0600` socket is the reach restriction — as it
already is for a definition route's JSON-RPC, which the socket serves
without a token. Over TCP, the listener's gate still demands a verified
client certificate or the bearer token before the route is looked up, so a
bare GET there is `401`, like every other uncredentialed request. A
listener without `client_ca` can therefore serve no remote card fetch: the
token never leaves the process.

The card path answers `GET` alone — anything else is `405` with
`Allow: GET` — and only for a route that exists: an unknown id is `404`, as
is every other sub-path of a route. The `Origin`, `Host` and `A2A-Version`
checks below apply to a card GET too; only the `Content-Type` gate is
skipped, since a GET carries no body.

## Endpoints

Every dispatch answers on the process-wide host under its own path. The
URL's host label, `crush-a2a`, never leaves the process: the client's
dialer maps it onto the unix socket.

| Request | Path | Behaviour |
| --- | --- | --- |
| `POST` | `/agents/<dispatch id>` | JSON-RPC 2.0. `SendStreamingMessage` and `SubscribeToTask` answer as an SSE stream; every other method answers with one JSON response. |
| `POST` | `/agents/<definition id>` | One route per enabled agent definition (`coder`, `plan`, `task`, `worker`, yours). Same handler, but no run is bound to it: a message is rejected with `no running agent for context <id>; task sessions are not continuable` until an entry point serves a turn on the route. Over TCP it still requires a credential first. |
| `GET` | `/agents/<id>/.well-known/agent-card.json` | The route's Agent Card, for a dispatch or a definition route: see [Fetching a card](#fetching-a-card). Any other method is `405` with `Allow: GET`. |
| `GET` | `/agents` | The agent index: see [below](#agent-index). Socket only. |
| anything else | any | `404`: an unknown id, and every other sub-path of a route. The host has no card of its own at `/.well-known/agent-card.json`. |

Middleware in front of the route table rejects a request before any
dispatch work runs: `403` when an `Origin` header is present, `415`
unless `Content-Type` parses to `application/json` — a card `GET` excepted,
since it carries no body — and `400` unless the `Host` is `crush-a2a` on
the socket or one the TCP listener answers to.

### Agent index

The host lists the dispatches it serves at `GET /agents`. This is how
a UI meets dispatched agents over the wire rather than through the
dispatch registry. Sub-agent turns are not listed. The index lives with
the process: a restart starts it empty, and a finished dispatch's
durable record stays in its session.

- **A snapshot.** With `Accept: application/json`, the answer is a JSON
  array of descriptors, oldest first.
- **A stream.** With `Accept: text/event-stream`, the answer is an SSE
  stream. Its first `data:` event is `{"snapshot": [...]}`, and every
  change after it is `{"upsert": {...}}`, one descriptor at a time. An
  idle stream carries a comment every 30 seconds. A stream that falls 256
  events behind, or whose reader stops taking writes for 10 seconds, is
  closed rather than holding up the agent or the host's shutdown. The
  watcher reconnects to a fresh snapshot.

A descriptor carries:
- the dispatch ID, its endpoint, and its Agent Card;
- its handle and role;
- its `context_id` (the child session) and `parent_session_id`;
- its `task_id`;
- its `state`: `working`, `completed`, `failed` or `canceled`, and empty
  before the task exists;
- the latest `status_text`;
- the latest `todos/v1` value as `progress`, and the latest `usage/v1`
  value as `usage`;
- `served`, and `started_at`, `updated_at` and `finished_at`;
- a `revision` that increases with every change on the index, so a
  watcher keeps the newer of two copies and repeats no update after a
  reconnect.

The descriptor follows the dispatch's own task only. The executor names
that task when the dispatch's turn starts, so the tasks that steers open
on the same context never move it, and a write that changes nothing a
reader sees is not published. A dispatch whose route has gone down stays
listed with `served: false` and its last state, so its `@handle` still
resolves; the oldest are dropped beyond 500 ended dispatches.

The index takes the host's checks without the content-type gate: `GET`
only (`405` otherwise), `403` with an `Origin`, `400` unless the `Host`
is `crush-a2a`, and the host's bearer token from the host's own user
(`401` otherwise).

In client/server mode the TUI runs in another process. It reaches the index,
and each dispatch's route, through the server's proxy at
`/v1/workspaces/{id}/a2a/agents`; see
[Dispatched agents against a server](/features/server-and-workspaces#dispatched-agents-against-a-server).
The server adds the host's token itself and refuses any request with an
`Origin`. The request keeps its `Content-Type` and version headers, so the
host judges those as the client sent them.

## TCP listener

The host can also listen on TCP. This is the transport groundwork for
agents on other hosts or in other sandboxes: an authenticated remote caller
reaches every route, but no run is served to it yet (see
[Local runs stay local](#local-runs-stay-local)). It is off unless you set
a listen address, and it only speaks TLS.
A certificate recipe and the team workflow are on
[Working with other agents](./team.md). A listen address without both a
certificate and a key fails the load with
`a2a.listen requires tls_cert and tls_key; plain TCP is not supported`.

```bash
# crushrc
option a2a-listen 127.0.0.1:7443
option a2a-tls-cert ~/.config/crush/certs/a2a.pem
option a2a-tls-key ~/.config/crush/certs/a2a-key.pem
option a2a-client-ca ~/.config/crush/certs/clients-ca.pem   # optional: require client certificates
```

```json
{
  "options": {
    "a2a": {
      "listen": "127.0.0.1:7443",
      "tls_cert": "~/.config/crush/certs/a2a.pem",
      "tls_key": "~/.config/crush/certs/a2a-key.pem",
      "client_ca": "~/.config/crush/certs/clients-ca.pem"
    }
  }
}
```

| Key | `crushrc` | Meaning |
| --- | --- | --- |
| `options.a2a.listen` | `option a2a-listen` | `host:port` to listen on. An empty host or `0.0.0.0` listens on every interface. |
| `options.a2a.tls_cert` | `option a2a-tls-cert` | PEM server certificate (chain). |
| `options.a2a.tls_key` | `option a2a-tls-key` | PEM private key for the certificate. |
| `options.a2a.client_ca` | `option a2a-client-ca` | Optional PEM CA certificates. When set, every client must present a certificate signed by one of them (mutual TLS). |

The three file paths must be absolute or start with `~/`. A relative path
fails the load: the listener usually lives in your global config, and a
relative path would resolve against whichever project Crush runs in. At load
the certificate and key must also load as a pair, and `client_ca` must hold
at least one PEM certificate, or the load fails and names the key.
Certificate provisioning, including ACME, is up to you.

How the listener behaves:

- **Authentication first.** TCP has no peer credentials, so a TCP call is
  never treated as the host's own user. Every request must arrive on a
  connection whose client certificate was verified against `client_ca`, or
  carry the host's bearer token. Anything else gets a `401` before its body
  is read or its route is looked up, so every path answers alike. The
  rejection is logged at warning level with the peer's address, never the
  credential it offered. Failed TLS handshakes are logged too.
- **Without `client_ca`.** The bearer token is then the only way in, and it
  never leaves the process. No remote client can authenticate, and Crush
  logs a warning at startup when such a listener is reachable beyond
  loopback.
- **Same routes, same middleware.** An authenticated request reaches the
  same `/agents/<id>` routes the socket serves, with the same `Origin`,
  `Content-Type` and version checks. The [agent index](#agent-index) is the
  exception: it lists every dispatch the host serves, so only the socket
  answers `GET /agents`. TLS 1.2 is the minimum. The handshake and request
  headers must complete within 30 seconds. A request body is capped at
  32 MiB, enough for a message carrying several full-size attachments. An
  idle keep-alive connection is closed after two minutes. Plain HTTP to the
  port fails at the TLS layer and never reaches a route.
- **Host check.** The `Host` header must be the listen address (as
  configured, or with the port the kernel picked for `:0`) or a DNS or IP
  name in the server certificate's subject alternative names, at any port.
  Anything else is a `400`.
- **Identity.** A certificate holder is identified by the fingerprint of
  the `client_ca` certificate that verified it plus the certificate's
  subject, and its tasks are stored under that identity. Holders of
  certificates with the same subject from the same CA share their tasks;
  anyone else's are invisible to them, even from a CA that reuses another
  CA's name.
- **Cards.** While the listener runs, every card lists its HTTPS interface
  second, after the socket one: `https://<host:port>/agents/<id>`. The host is
  the configured one. With a wildcard listen address it is the server
  certificate's first DNS name that is not itself a wildcard, else its first
  IP address. With neither, the cards list no HTTPS interface and Crush logs
  a warning. With `client_ca` the card also declares the `crush-mtls`
  scheme. Crush's own dispatch client keeps dialing the socket. An
  authenticated caller can fetch any route's card at
  `/agents/<id>/.well-known/agent-card.json`; the copy it gets lists one
  interface, the route at the origin the caller dialed, so a client that
  takes the first interface it supports lands on this listener. See
  [Fetching a card](#fetching-a-card).
- **Lifetime.** The listener starts with the host and shuts down with it,
  alongside the socket. Connections that have not sent a request are closed
  at once, and a request still running at the shutdown deadline is cut. If
  the listener cannot start, for example because the port is taken, Crush
  logs the error and keeps serving the socket alone; cards then list no
  HTTPS interface. Changing the settings takes a restart.

### Local runs stay local

The TCP listener exposes no local dispatch or sub-agent run. Every run
Crush starts in this process belongs to the local user, and a TCP call
that names its context gets the same rejection as an unknown context:
`no running agent for context <id>`. It can neither message, steer, answer
nor cancel it, even with a valid credential. Agents that remote clients can
call arrive with the peer registry
([#334](https://github.com/joestump-agent/crush/issues/334)).

## Methods

The SDK's default request handler serves the whole A2A method set. Crush's
client calls four of them: `SendStreamingMessage` for the dispatch, its
steers and its answers, `CancelTask` for kills, and — when a stream drops
mid-run — the two resume methods.

| Method | Crush calls it | What the server does |
| --- | --- | --- |
| `SendStreamingMessage` | Yes: the dispatch, steers, and one per answered question | Runs the turn, delivers the steer, or resumes the parked run, and streams the events below. |
| `SendMessage` | No | Runs the turn and returns the final result in one response. |
| `GetTask`, `ListTasks` | `GetTask` on resume | Answer from the SQLite task store, scoped to the caller's identity. |
| `CancelTask` | Yes — wander kills, user cancels, and questions no answer is coming for | Calls the agent's `Cancel` and emits `TASK_STATE_CANCELED`, the request's reason as the status message. |
| `SubscribeToTask` | On resume | Re-attaches to a live task's stream. |
| Push-notification config methods | No | Return "push notifications not supported". |
| `GetExtendedAgentCard` | No | Returns "extended card not configured". |

### The request

The coordinator sends one user message whose only part is the dispatch
prompt. It sets `contextId` to the dispatch's task session ID and leaves
`taskId` unset, so the server assigns one task per run.

```json
{
  "jsonrpc": "2.0",
  "id": "5d0c…",
  "method": "SendStreamingMessage",
  "params": {
    "message": {
      "messageId": "9a41…",
      "role": "ROLE_USER",
      "contextId": "task-session…",
      "parts": [{ "text": "Add table-driven tests for pkg/x…" }]
    }
  }
}
```

The `contextId` is the Crush task session: the server rejects any message
whose context does not name the session its dispatch serves, including
one whose run has ended — task sessions are not continuable. The
`taskId` identifies one run: each turn is a new task, and the terminal
status echoes it as the run ID.

The executor joins every text part of the message with newlines. Any
other kind of part is carried as an attachment — a steer delivers it to
the running agent through the same attachment pipeline a typed prompt
takes; the dispatch's own single-part message has none.

### Steering

The first message on a context is the dispatch's own turn. Every later
message on that context is a **steer**: the server hands it to the
running agent through the same queue the in-process front doors use
(`@handle`, `message_agent`) — it never starts a turn, and the persisted
message is marked as a steer. `referenceTasks` may name the task being
steered; they are advisory and never validated.

The steer is its own task, terminal on the queue's verdict:

| Outcome | State | Status message |
| --- | --- | --- |
| The running agent consumed the message — folded into its active turn or picked up as its follow-up turn | `TASK_STATE_COMPLETED` | `delivered` |
| The queue dropped it without running: the run ended — failed, canceled or killed — while the steer sat queued | `TASK_STATE_FAILED` | `agent finished before the message was consumed` |
| The run is live but its session is not taking messages yet: the run is starting, or between its turn and a context compaction | `TASK_STATE_REJECTED` | `agent is not ready for messages yet; send the message again in a moment`, with `{"reason": "not_ready"}` under the `steer-refusals/v1` extension in the status metadata |
| The run has ended — task sessions are not continuable | `TASK_STATE_REJECTED` | `agent is no longer running; task sessions are not continuable` |

The steer's reply streams on the dispatch's own surfaces — the parent's
dispatch block, `@handle` inspection — never on the steer's task.

A steer is accepted once it is queued, and that is when `message_agent`
returns ("queued"). A run that ends before reading a queued steer drops it:
the dispatch's terminal status names every steer the agent accepted but
never read under the `undelivered-steers/v1` extension
(`{"steers": [...]}`), and the parent's result lists them as
`undelivered_steers`. A steer the agent read — folded into a step or
run as a follow-up turn — is never listed, even if the run then
fails.

### Questions

A dispatched agent can ask the parent's user a question. It gets the
`question` tool when the parent session is interactive at dispatch time,
and the tool asks through the dispatch's own question service, never the
parent's:

1. **The pause.** The agent's question parks the run: the agent stays
   blocked inside its tool call, and the stream ends with
   `TASK_STATE_INPUT_REQUIRED`. The status message carries the question
   texts as a text part and the whole request as a data part — the
   declared `questions/v1` extension, named in the message's
   `extensions`:

   ```json
   {
     "statusUpdate": {
       "status": {
         "state": "TASK_STATE_INPUT_REQUIRED",
         "message": {
           "role": "ROLE_AGENT",
           "extensions": ["https://crush.charm.land/ext/questions/v1"],
           "parts": [
             { "text": "Which database?" },
             { "data": { "id": "…", "session_id": "…", "tool_call_id": "…",
               "questions": [
                 { "id": "…", "type": "free_text", "question": "Which database?",
                   "description": "The schema differs per engine." }
               ] } }
           ]
         }
       }
     }
   }
   ```

2. **The question.** The parent's coordinator shows it in your question
   prompt, each question prefixed with the dispatch's `@handle`.
   Dispatched questions take turns: one is on screen at a time. With no
   interactive user — a non-interactive parent, or a transport with no
   question handler — every question is answered at once with
   `no interactive user; proceed with your best judgment`, and the agent
   carries on. Dismissing the prompt answers
   `the user declined to answer; proceed with your best judgment`.
3. **The answer.** The client sends a user message with the parked
   task's `taskId`, whose data part is the declared `answers/v1`
   extension: `{"answers": [...]}`, one answer per question ID. A
   message with no answers data counts its text as a free-text answer to
   every question. The task goes back to `TASK_STATE_WORKING`, the same
   run resumes — no new turn starts — and the new stream runs to the
   terminal state or the next question.

An answer and a steer are told apart explicitly. A message whose `taskId`
names a task in `TASK_STATE_INPUT_REQUIRED` is an answer; with no
question or permission request pending on that task it is
`TASK_STATE_REJECTED` ("no question or permission request is pending on
this task") and starts nothing. Every other message on the
context is a turn or a steer, as above, so a steer sent while a question
is open is still only a steer, and the question stays open.

While a run is parked there is no execution, so neither the SDK's
inactivity guard nor the executor's backstop runs, but `hard_timeout`
and `stall_window` keep counting. A kill ends the wait on your answer:
the client cancels the parked task with the kill reason, the parked tool
call returns an error, and the task ends `TASK_STATE_CANCELED`. A
`CancelTask` on a parked task does the same.

### Permission prompts

A dispatched agent's tool calls ask for permission through the dispatch's
own scoped permission service. Yolo and the
allowlists resolve inside that service, so only a request that needs a
person ever reaches the protocol. Such a request parks the run exactly like
a question:

1. **The pause.** The stream ends with `TASK_STATE_INPUT_REQUIRED`. The
   status message carries a one-line summary as a text part and the
   request as a data part, the declared `permissions/v1` extension, named
   in the message's `extensions`:

   ```json
   {
     "role": "ROLE_AGENT",
     "extensions": ["https://crush.charm.land/ext/permissions/v1"],
     "parts": [
       { "text": "Permission required: bash: Execute command: make test" },
       { "data": { "id": "…", "session_id": "…", "tool_call_id": "…",
         "tool_name": "bash", "description": "Execute command: make test",
         "action": "execute", "params": { "command": "make test" },
         "path": "/…/worktree" } }
     ]
   }
   ```

2. **The decision.** The parent's coordinator requests it on the parent's
   own permission service, for the dispatch's own session, with the
   dispatch's `@handle` in front of the description. That service applies your live yolo setting and the grants
   you made for the session before it shows the dialog. The tool's params
   decode back to the tool's own type, so the dialog renders a dispatched
   request like a local one; an MCP tool's raw input stays a string. The
   dialog shows the request's path, the dispatch's worktree, while it
   shows the description only for some tools. With no handler on the client, the request is
   denied.
3. **The answer.** The client sends a user message with the parked task's
   `taskId`, whose data part is the declared `permission-decisions/v1`
   extension: `{"allow": true}` or `{"allow": false}`. Only an explicit
   allow grants the request; a message with no decodable decision denies
   it. The same run resumes, and the tool call runs or returns a denial.

Requests from parallel tool calls park one at a time: the scoped
service publishes the next request only once the parked one is decided,
and the run resumes and parks again on it. A kill
while a request is parked ends the wait the same way it does for a
question. A `CancelTask` on a task parked on a request ends the task
`TASK_STATE_CANCELED`, and the parked tool call gets a denial or the
canceled run's context error, whichever lands first; it never runs.

## Event stream

Each SSE `data:` line is a JSON-RPC response whose `result` holds exactly
one of `task`, `statusUpdate` or `artifactUpdate`. A successful dispatch
streams:

| # | Event | State | Content |
| --- | --- | --- | --- |
| 1 | `task` | `TASK_STATE_SUBMITTED` | The new task, with its IDs. |
| 2 | `statusUpdate` | `TASK_STATE_WORKING` | No message. The run has started. |
| 3 | `statusUpdate` × 0..n | `TASK_STATE_WORKING` | One per todo-list change: the current todo as message text, the typed progress under the declared `todos/v1` extension's metadata key, and the usage so far under `usage/v1`. |
| 4 | `artifactUpdate` × 1..n | — | The work product: the diff as chunked `text/x-diff` parts (artifact `diff`), then the typed outcome as a data part (artifact `dispatch-result`). |
| 5 | `statusUpdate` | `TASK_STATE_COMPLETED` | The agent's final text as an agent message. |

A question the agent asks ends a stream early, with
`TASK_STATE_INPUT_REQUIRED` after row 3; the answer's stream starts at
row 2 on the same task. See [Questions](#questions).

### Todo progress events

A progress event looks like this:

```json
{
  "statusUpdate": {
    "taskId": "…",
    "contextId": "…",
    "status": {
      "state": "TASK_STATE_WORKING",
      "message": {
        "role": "ROLE_AGENT",
        "parts": [{ "text": "Writing table-driven tests" }]
      }
    },
    "metadata": {
      "https://crush.charm.land/ext/todos/v1": {
        "current": "Writing table-driven tests",
        "completed": 1,
        "total": 2,
        "todos": [
          { "content": "Read pkg/x", "status": "completed", "activeForm": "Reading pkg/x" },
          { "content": "Write tests", "status": "in_progress", "activeForm": "Writing table-driven tests" }
        ]
      }
    }
  }
}
```

- **Message text.** The first `in_progress` todo's `activeForm`, else its
  `content`. With nothing in progress, the text is `N/M completed`.
- **The `todos/v1` extension value.** A `TodoProgress` object: `current`
  (the in-progress todo's active form or content, empty when none),
  `completed` and `total` counts, and `todos` — every todo with `status`
  set to `pending`, `in_progress` or `completed`. The card declares the
  extension under `capabilities.extensions`, with its JSON Schema in the
  extension's params. Metadata keys that are not declared extensions are
  dropped by the client, never fatal.
- **When an event is sent.** Only when the list actually changed and is
  not empty. Saves that change only usage stay silent.
- **Loss.** Delivery is lossy under back-pressure, through a 16-slot
  buffer per subscriber. A dropped snapshot is superseded by the next one.
- **Ordering.** Progress events and the terminal status are yielded from
  one goroutine, so no progress event follows a terminal one.

### Usage and trace context

Every post-run terminal status — `TASK_STATE_COMPLETED`, both
`TASK_STATE_FAILED` paths and an out-of-band `TASK_STATE_CANCELED` —
carries the declared `usage/v1` extension's metadata key. Each todo
progress event carries it too, with the usage so far, so a watcher's
token count moves while the agent works:

```json
{
  "https://crush.charm.land/ext/usage/v1": {
    "model": "claude-opus-5",
    "provider": "anthropic",
    "promptTokens": 1200,
    "completionTokens": 340,
    "cost": 0.042,
    "traceId": "4bf92f3577b34da6a3ce929d0e0e4736"
  }
}
```

- **The totals.** The child session's cumulative token counts and cost at
  terminal time, plus the model and provider that served the dispatch.
  Todo progress events carry the totals so far, for watchers only: the
  parent is charged a terminal status's usage alone. A parent-requested
  cancel carries none, because the totals are not final.
- **The trace id.** The parent stamps a W3C `traceparent` header on the
  dispatch call (a client interceptor sends it, the server propagator
  lifts it), and the executor echoes its trace-id segment back in
  `traceId`, so server-side dispatch logs and the parent's client call
  share one correlation id. When the parent sent none, the server mints
  one for its own logs.
- **Failure.** If the totals cannot be read, the status ships without
  metadata and the run's outcome is untouched; the parent leaves its
  recorded cost unchanged rather than guessing.
- **Limitation.** Usage rides status-update events only: a terminal state
  recovered through `tasks/get` or `tasks/resubscribe` (the resume path)
  carries no metadata.

### The artifacts

Two artifacts carry the work product.

**`diff`.** Emitted only after a successful run, and only when the diff
is non-empty and could be captured. It is the workspace's diff against the
merge-base of its base and `HEAD`, falling back to the base SHA recorded at
provision. It covers committed, uncommitted and untracked files. The diff
streams as `text/x-diff` chunks of at most 256 KiB, named `dispatch.diff`:
the first chunk creates the artifact, the rest append, and the last closes
it, so no message can exceed the transport's line limits however large the
diff grows. It is sent in full: the coordinator's line and byte cuts are
applied later, when it builds `diff_summary`.

**`dispatch-result`.** Emitted on every completed run, with or without a
diff. One data part carries the typed outcome: `diffBytes`,
`filesChanged`, and `diffError` when the diff could not be captured. A
diff error does not fail the run: the task still completes, and the error
rides the result.

### Other endings

| Ending | What is streamed |
| --- | --- |
| Message has no text | `TASK_STATE_REJECTED`, "message has no text to run". Crush's client refuses an empty prompt before sending, so only another client can hit this. |
| Run returns an error | `TASK_STATE_FAILED` with the error text. |
| Agent was busy, or a cancel landed at start | `TASK_STATE_FAILED`, "agent session did not start a turn (busy or canceled)". The prompt is still queued on the session, and the running agent reads it. |
| `CancelTask` | `TASK_STATE_CANCELED`, the request's reason as the status message. |
| The agent asks a question | `TASK_STATE_INPUT_REQUIRED` with the question; not an end. The answer resumes the run on the same task. |
| An answer on a task with nothing pending | `TASK_STATE_REJECTED`, "no question or permission request is pending on this task". |

### How the client maps the outcome

| Stream outcome | `DispatchResult` |
| --- | --- |
| `TASK_STATE_COMPLETED` | `completed`. `key_findings` is the message text. `diff_summary` comes from the `diff` artifact: `(no changes)` when it is empty, `(diff unavailable: …)` when the result artifact carries a `diffError`. |
| `TASK_STATE_FAILED`, `TASK_STATE_REJECTED` | `failed`. `error` is the message text. |
| `TASK_STATE_CANCELED` | A status message that is a kill reason maps through the kill assembly: `killed`, with that reason. Anything else is `failed`, with `error` "dispatch canceled: …". |
| Stream error the resume cannot recover | `failed`, with `error` "a2a: dispatch stream: …". |
| Stream ends with no terminal state | `failed`, with "…ended without a terminal state". |

### Dropped streams

The stream's first event names the task, and the coordinator records that
ID on the dispatch registry entry. When the connection drops before a
terminal state, the task is still running — or already finished — on the
server, so the client resumes instead of failing the dispatch:

- Up to three attempts, 250ms, 1s and 4s apart. Each calls
  `SubscribeToTask`, which replays the stored task snapshot — the
  authoritative state of the artifacts received before the cut, so a
  partial diff can never double — and then the live events.
- When the execution has already ended, `SubscribeToTask` answers
  "task not found" and the client calls `GetTask` instead: a terminal
  state folds and lands, a task parked in `TASK_STATE_INPUT_REQUIRED`
  hands its question on to be answered, and any other state retries.
- A terminal state wins wherever it comes from, and the replayed
  snapshot never counts as progress, so a Working event is never
  counted twice.
- When every attempt is exhausted the dispatch fails with the original
  stream error, and the orphaned run is canceled before teardown.

A terminal `task` snapshot is accepted when no terminal status event
arrived. `Working` events are counted, not re-published: the agent block
renders from the in-process todo collector.

### Kills

Every kill — the wander watchdog's hard timeout and stall kill, the todo
ladder's kill rung, and the user's cancel — records its reason on the
run's kill state, then sends one `CancelTask` carrying it:

- The reason rides the request's metadata under `crush.dispatch.cancel_reason`.
- The send is asynchronous: the SDK resolves a `tasks/cancel` only when
  the run ends, so the kill site returns to its loop immediately and the
  canceler's events land on the execution's own queue.
- The executor decodes the metadata and puts the reason on the terminal
  `TASK_STATE_CANCELED` status message — it reaches the parent on the
  live stream and lands in the task store, so an out-of-process kill
  reads the same as an in-process one.
- The parent maps a `TASK_STATE_CANCELED` whose status message is a kill
  reason onto `killed` through the kill assembly.
- The direct in-process cancel remains as the fallback for the paths no
  `tasks/cancel` can serve: an unserved dispatch, a kill that lands
  before the stream has named the task ID, or a cancel that errors. First
  reason wins; a late kill after natural completion is discarded.

## Durable state

Served tasks live in the `a2a_tasks` table of the session database, through
a SQLite implementation of the SDK's task store, so `GetTask` and the resume
path read the same state after a restart. Every dispatch also leaves one
durable record in `a2a_dispatches`: written at start, updated with the
terminal result, and stamped delivered when the parent's delivery turn
succeeds. On startup, before the UI loads a session, the reconciler fails
every task and dispatch record whose owning process died — the dispatch error
names the preserved workspace — and re-delivers every terminal result the
parent never received, stamping it on the parent's persisted `dispatch_agent`
tool result. That stamp is what a reloaded agent block renders.

## Not implemented

- **`auth-required`.** a2a-go v2.5.0 treats it as non-final, so an answer on
  the same task is refused while the execution stays active. Permission
  prompts use `input-required` instead; see
  [Permission prompts](#permission-prompts).
- **Runs for remote callers.** A TCP caller reaches every route and is
  rejected by every one; see [Local runs stay local](#local-runs-stay-local).

## Security model

:::info[Bearer-authenticated, socket-reachable]
Every served call must carry the host's bearer token, declared on the
card as a `crush-bearer` HTTP bearer `securitySchemes` entry. The dispatch
client attaches it automatically; anything else is rejected with the
JSON-RPC unauthenticated error before the handler runs. The reach
restriction is the socket, layered under the credential:

- **The credential.** Each host mints 32 bytes of `crypto/rand` at bind
  time and holds it only in memory — never persisted, never logged. The
  check is constant-time. Where the platform reports socket peer
  credentials, the peer must also be the host's own OS user, and tasks
  are stored under that identity.
- **Who can reach it.** Only processes running as the same OS user: the
  socket is `0600` inside a `0700` directory, `<data dir>/a2a/`. When
  that path would overflow the unix socket length limit, the host falls
  back to `crush-a2a-<uid>/` under `$XDG_RUNTIME_DIR`, or under the temp
  directory when that is unset. Either directory is verified before the
  bind ([#558](https://github.com/joestump-agent/crush/issues/558)): a
  symlink or non-directory at its path, or a directory owned by another
  user, is refused with an error naming the path and the failed check,
  and a directory left wider than `0700` is tightened. The socket is
  then made `0600` without following symlinks. Other local users cannot
  connect to the socket. The host listens on TCP only when you configure
  the [TCP listener](#tcp-listener), and then only with TLS
  ([#358](https://github.com/joestump-agent/crush/issues/358)).
- **TCP callers.** A TCP connection carries no peer credentials, so a TCP
  call is never treated as the local user: it needs the bearer token or,
  with `client_ca`, a client certificate the handshake verified, checked
  before its body is read. Its `Host` must be the listen address or a name
  in the server certificate. Even authenticated, it reaches no local run
  ([Local runs stay local](#local-runs-stay-local)).
- **Browsers.** The host rejects cross-origin requests (`403`), non-JSON
  bodies including the CORS-simple `text/plain` POST (`415`), and a
  `Host` other than the internal `crush-a2a` label on the socket, or one
  the TCP listener does not answer to (`400`), so a web page cannot fold a
  prompt into a running dispatch.
- **The card.** A route's Agent Card is discovery metadata, which A2A
  clients fetch with a bare `GET`. On the socket it is served without the
  token — the `0600` socket is the reach restriction, as for a definition
  route's JSON-RPC. Over TCP the gate still demands a client certificate or
  the token first. The `Origin` and `Host` checks apply to it as to every
  request. See [Fetching a card](#fetching-a-card).

A same-user process can no longer steer a dispatch: without the token,
which only the host process holds, every call is rejected.

To turn dispatch off, run `permissions deny dispatch_agent`.
:::

## External agents

A `runtime: a2a` dispatch agent is the one case where Crush is the client of
someone else's A2A server. The wire
differs from a served dispatch in these ways:

- The card comes from `GET` on the configured URL, with no `Authorization`
  header, and must offer `JSONRPC` on the card URL's own origin.
- Every call after that carries `Authorization: Bearer <token>` when the
  definition sets `auth`, and goes only to that origin.
- The prompt is a `SendStreamingMessage` with no `contextId` and no
  activated extensions, and its response headers must arrive within 30
  seconds. A card that declares `streaming: false` gets a blocking
  `SendMessage` instead, bounded by the idle timeout. Extension metadata on
  the stream is ignored.
- A bare `Message` reply completes the dispatch. Artifacts are read as text,
  never as the `diff` and `dispatch-result` artifacts.
- The first task ID must be 256 or fewer printable characters, or the stream
  is refused.
- `INPUT_REQUIRED` and `AUTH_REQUIRED` are answered with `CancelTask`, never
  forwarded to the user.
- A kill, or a stream silent past `transport.idle_timeout` (5 minutes unless
  configured), ends the stream. Once the remote has named its task, Crush
  sends a best-effort `CancelTask` carrying the reason under
  `crush.dispatch.cancel_reason`; before that, there is no task to cancel.
- Response bodies stop at 16 MiB and one server-sent event at 4 MiB.

Configuration and the trust model are in
[External agents](./configuration.md#external-agents).

## What is next

Everything the 2026-10-04 re-base planned for the protocol has shipped: one
path through the client with no direct-run fallback, `contextId` as the
session and `taskId` as the run, steering as a message on the context,
questions and permissions as `input-required` pauses, kills as
`tasks/cancel`, declared extensions, the chunked diff artifact and typed
result, usage and trace metadata, the SQLite task store with startup
reconciliation, the TLS listener, and the a2a-go TCK in CI. The reasoning
is in [Design decisions](./design-decisions.md).

What remains is the peer registry
([#334](https://github.com/joestump-agent/crush/issues/334)): the piece that
lets a remote, authenticated caller reach a run on this host and that
publishes the definition cards it can dial. Until it lands, a Crush host is a
secure dead end for other agents, and the way to work with another agent is
to [consume it](./configuration.md#external-agents).
