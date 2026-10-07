---
id: a2a-protocol
title: A2A wire reference
sidebar_label: A2A protocol
description: What each dispatched agent's A2A server exposes today — the Agent Card, endpoints, the JSON-RPC methods Crush calls, the event stream and its todo metadata, the security model, and the planned protocol surface.
---

# A2A wire reference

:::info[Fork feature]
The A2A layer is an addition in the `joestump-agent/crush` fork.
:::

Every dispatched agent is served over the
[A2A protocol](https://a2a-protocol.org) while it runs. This page documents
exactly what is on the wire today, so you can debug it, read logs against
it, or plan against it. How the server fits into a dispatch is on
[Architecture](./architecture.md).

:::warning[Internal interface]
External A2A clients are not a supported interface. Each endpoint exists
only while one dispatch runs, its port changes every time, the UI never
shows it, and nothing authenticates callers. The only supported client is
Crush's own coordinator.
:::

## At a glance

| Item | Today |
| --- | --- |
| SDK | [`github.com/a2aproject/a2a-go/v2`](https://github.com/a2aproject/a2a-go) v2.5.0 |
| Protocol version | `1.0`, stamped on the card's interface |
| Binding | JSON-RPC 2.0 over HTTP; streaming responses are Server-Sent Events |
| Listener | One host per process on a unix socket: `<data dir>/a2a/<pid>.sock`, socket mode `0600` in a `0700` directory |
| Lifetime | From just after provisioning until the run returns; 5-second graceful shutdown |
| Discovery | In memory: endpoint and card are stamped on the dispatch registry entry |
| Task store | The SDK's in-memory store, one per server |
| Authentication | None |

Method names below are the A2A 1.0 names the SDK uses. Older A2A material
calls `SendStreamingMessage` `message/stream`, and `CancelTask`
`tasks/cancel`.

## Agent Card

The coordinator builds the card from the dispatch and stamps it on the
registry entry; discovery is in-memory and the card is not served over
the wire:

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
| `name` | The dispatch's assigned `@handle`, without the `@`. |
| `description` | The dispatch's `role`, which may be empty. |
| `version` | The Crush build version. |
| `supportedInterfaces[0]` | The routed endpoint, `http://crush-a2a/agents/<dispatch id>`, JSON-RPC binding, protocol `1.0`. Always exactly one. |
| `capabilities` | `streaming: true` and the declared `extensions`: `todos/v1`, `usage/v1`, `questions/v1` and `answers/v1`, each with its JSON Schema in its params. No push notifications, no extended card. |
| `defaultInputModes`, `defaultOutputModes` | `text/plain` both ways. |
| `skills` | One entry per Crush skill the dispatch was given: every discovered skill when the dispatch named none. `id` and `name` are the skill name, and every entry carries the single tag `crush-skill`. |

The card declares no `securitySchemes` and no `provider`. The coordinator
never fetches the card over HTTP. It reads the same object from the
registry entry.

## Endpoints

Every dispatch answers on the process-wide host under its own path. The
URL's host label, `crush-a2a`, never leaves the process: the client's
dialer maps it onto the unix socket.

| Request | Path | Behaviour |
| --- | --- | --- |
| `POST` | `/agents/<dispatch id>` | JSON-RPC 2.0. `SendStreamingMessage` and `SubscribeToTask` answer as an SSE stream; every other method answers with one JSON response. |
| `GET` | `/agents` | The agent index: see [below](#agent-index). |
| anything else | any | `404`. The well-known card path is not served; discovery is in-memory. |

Middleware in front of the route table rejects a request before any
dispatch work runs: `403` when an `Origin` header is present, `415`
unless `Content-Type` parses to `application/json`, and `400` unless the
`Host` is `crush-a2a`.

### Agent index

The host lists the dispatches it serves at `GET /agents`
([#421](https://github.com/joestump-agent/crush/issues/421)). This is how
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
The server adds the host's token itself. The request keeps its `Origin`,
`Content-Type` and version headers, so the checks above judge it as the
client sent it.

## Methods

The SDK's default request handler serves the whole A2A method set. Crush's
client calls four of them: `SendStreamingMessage` for the dispatch, its
steers and its answers, `CancelTask` for kills, and — when a stream drops
mid-run — the two resume methods.

| Method | Crush calls it | What the server does |
| --- | --- | --- |
| `SendStreamingMessage` | Yes: the dispatch, steers, and one per answered question | Runs the turn, delivers the steer, or resumes the parked run, and streams the events below. |
| `SendMessage` | No | Runs the turn and returns the final result in one response. |
| `GetTask`, `ListTasks` | `GetTask` on resume | Answer from the per-server in-memory store. |
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
| The queue dropped it without running (the run was canceled while the steer sat queued) | `TASK_STATE_FAILED` | `agent finished before the message was consumed` |
| The run has ended — task sessions are not continuable | `TASK_STATE_REJECTED` | `agent is no longer running; task sessions are not continuable` |

The steer's reply streams on the dispatch's own surfaces — the parent's
dispatch block, `@handle` inspection — never on the steer's task.

### Questions

A dispatched agent can ask the parent's user a question
([#352](https://github.com/joestump-agent/crush/issues/352)). It gets the
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
question pending on that task it is `TASK_STATE_REJECTED` ("no question
is pending on this task") and starts nothing. Every other message on the
context is a turn or a steer, as above, so a steer sent while a question
is open is still only a steer, and the question stays open.

While a run is parked there is no execution, so neither the SDK's
inactivity guard nor the executor's backstop runs, but `hard_timeout`
and `stall_window` keep counting. A kill ends the wait on your answer:
the client cancels the parked task with the kill reason, the parked tool
call returns an error, and the task ends `TASK_STATE_CANCELED`. A
`CancelTask` on a parked task does the same.

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
carries the declared `usage/v1` extension's metadata key
([#364](https://github.com/joestump-agent/crush/issues/364)). Each todo
progress event carries it too, with the usage so far, so a watcher's
token count moves while the agent works
([#421](https://github.com/joestump-agent/crush/issues/421)):

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
| An answer on a task with no question pending | `TASK_STATE_REJECTED`, "no question is pending on this task". |

### How the client maps the outcome

| Stream outcome | `DispatchResult` |
| --- | --- |
| `TASK_STATE_COMPLETED` | `completed`. `key_findings` is the message text. `diff_summary` comes from the `diff` artifact: `(no changes)` when it is empty, `(diff unavailable: …)` when the result artifact carries a `diffError`. |
| `TASK_STATE_FAILED`, `TASK_STATE_REJECTED` | `failed`. `error` is the message text. |
| `TASK_STATE_CANCELED` | A status message that is a kill reason (#316) maps through the kill assembly: `killed`, with that reason. Anything else is `failed`, with `error` "dispatch canceled: …". |
| Stream error the resume cannot recover | `failed`, with `error` "a2a: dispatch stream: …". |
| Stream ends with no terminal state | `failed`, with "…ended without a terminal state". |

### Dropped streams

The stream's first event names the task, and the coordinator records that
ID on the dispatch registry entry. When the connection drops before a
terminal state, the task is still running — or already finished — on the
server, so the client resumes instead of failing the dispatch
([#349](https://github.com/joestump-agent/crush/issues/349)):

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
  stream error, and the orphaned run is canceled before teardown
  ([#344](https://github.com/joestump-agent/crush/issues/344)).

A terminal `task` snapshot is accepted when no terminal status event
arrived. `Working` events are counted, not re-published: the agent block
renders from the in-process todo collector.

### Kills

Every kill — the wander watchdog's hard timeout and stall kill, the todo
ladder's kill rung, and the user's cancel — records its reason on the
run's kill state, then sends one `CancelTask` carrying it
([#348](https://github.com/joestump-agent/crush/issues/348)):

- The reason rides the request's metadata under `crush.dispatch.cancel_reason`.
- The send is asynchronous: the SDK resolves a `tasks/cancel` only when
  the run ends, so the kill site returns to its loop immediately and the
  canceler's events land on the execution's own queue.
- The executor decodes the metadata and puts the reason on the terminal
  `TASK_STATE_CANCELED` status message — it reaches the parent on the
  live stream and lands in the task store, so an out-of-process kill
  reads the same as an in-process one.
- The parent maps a `TASK_STATE_CANCELED` whose status message is a kill
  reason onto `killed` through the kill assembly (#343).
- The direct in-process cancel remains as the fallback for the paths no
  `tasks/cancel` can serve: an unserved dispatch, a kill that lands
  before the stream has named the task ID, or a cancel that errors
  (#430). First reason wins; a late kill after natural completion is
  discarded.

:::warning[Known issue]
A panic inside the run crashes Crush ([#345](https://github.com/joestump-agent/crush/issues/345)).
:::

## Not implemented

- **`auth-required`.** Permission prompts use an in-process bridge
  ([#353](https://github.com/joestump-agent/crush/issues/353)).
  Questions use `input-required`; see [Questions](#questions).
- **Durable task state.** A restart loses every task ([#354](https://github.com/joestump-agent/crush/issues/354)).

## Security model

:::info[Bearer-authenticated, socket-reachable]
Every served call must carry the host's bearer token, declared on the
card as a `crush-bearer` HTTP bearer `securitySchemes` entry
([#357](https://github.com/joestump-agent/crush/issues/357)). The dispatch
client attaches it automatically; anything else is rejected with the
JSON-RPC unauthenticated error before the handler runs. The reach
restriction is the socket, layered under the credential:

- **The credential.** Each host mints 32 bytes of `crypto/rand` at bind
  time and holds it only in memory — never persisted, never logged. The
  check is constant-time. Where the platform reports socket peer
  credentials, the peer must also be the host's own OS user, and tasks
  are stored under that identity.
- **Who can reach it.** Only processes running as the same OS user: the
  socket is `0600` inside a `0700` directory under the data directory
  (a per-user temp dir when the path would overflow the socket length
  limit). Other local users cannot connect, and the socket never
  listens on TCP.
- **Browsers.** The host rejects cross-origin requests (`403`), non-JSON
  bodies including the CORS-simple `text/plain` POST (`415`), and a
  `Host` other than the internal `crush-a2a` label (`400`), so a web
  page cannot fold a prompt into a running dispatch.

A same-user process can no longer steer a dispatch: without the token,
which only the host process holds, every call is rejected. TCP with
mutual TLS remains planned ([#358](https://github.com/joestump-agent/crush/issues/358)).

To turn dispatch off, run `permissions deny dispatch_agent`.
:::

## Planned protocol surface

The 2026-10-04 re-base makes A2A carry the whole agent lifecycle. Stage 1
holds ship-blockers, stage 2 must land before
[Clustered Crush (#331)](https://github.com/joestump-agent/crush/issues/331),
and stage 3 comes after. The reasoning is in
[Design decisions](./design-decisions.md).

| Ticket | Change | Stage |
| --- | --- | --- |
| [#347](https://github.com/joestump-agent/crush/issues/347) | Delete the direct-run fallback; a server start failure fails the dispatch. | 2 |
| [#350](https://github.com/joestump-agent/crush/issues/350) | `contextId` maps to the Crush session, `taskId` to one run. | 2 |
| [#351](https://github.com/joestump-agent/crush/issues/351) | Steering is an A2A message on the running context, completed once consumed. | 2 |
| [#354](https://github.com/joestump-agent/crush/issues/354) | A durable SQLite implementation of the SDK's task store. | 2 |
| [#355](https://github.com/joestump-agent/crush/issues/355) | Wire the durable store; on startup, fail orphaned tasks and deliver undelivered results. | 2 |
| [#357](https://github.com/joestump-agent/crush/issues/357) | `securitySchemes` on the card; auth interceptors on server and client. | 2 |
| [#359](https://github.com/joestump-agent/crush/issues/359) | Declared, statically typed extensions (todos first); undeclared metadata keys rejected. | 2 |
| [#360](https://github.com/joestump-agent/crush/issues/360) | The SDK's agent inactivity timeout as a stall backstop. | 2 |
| [#361](https://github.com/joestump-agent/crush/issues/361) | The diff as a chunked `text/x-diff` file artifact, plus a summary and a typed `DispatchResult`. | 2 |
| [#364](https://github.com/joestump-agent/crush/issues/364) | Usage, cost, model and `traceparent` in task metadata. | 2 |
| [#352](https://github.com/joestump-agent/crush/issues/352) | Questions from dispatched agents through `input-required`. | 3 |
| [#353](https://github.com/joestump-agent/crush/issues/353) | Permission prompts through `auth-required`, answered by the client holder. | 3 |
| [#358](https://github.com/joestump-agent/crush/issues/358) | Optional TCP listener, TLS required, `Host` validated. | 3 |
| [#356](https://github.com/joestump-agent/crush/issues/356) | Spike: can the SDK's cluster mode carry Clustered Crush peers? | 3 |
| [#363](https://github.com/joestump-agent/crush/issues/363) | Run the a2a-go TCK against Crush's served card in CI. | 3 |

Everything is tracked on [#341](https://github.com/joestump-agent/crush/issues/341).
