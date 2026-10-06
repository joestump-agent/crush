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
| Listener | `127.0.0.1`, ephemeral port, one server per dispatch |
| Lifetime | From just after provisioning until the run returns; 5-second graceful shutdown |
| Discovery | In memory: endpoint and card are stamped on the dispatch registry entry |
| Task store | The SDK's in-memory store, one per server |
| Authentication | None |

Method names below are the A2A 1.0 names the SDK uses. Older A2A material
calls `SendStreamingMessage` `message/stream`, and `CancelTask`
`tasks/cancel`.

## Agent Card

`GET /.well-known/agent-card.json` returns the card. Crush builds it from
the dispatch:

```json
{
  "name": "tester",
  "description": "tester",
  "version": "devel",
  "supportedInterfaces": [
    {
      "url": "http://127.0.0.1:53817",
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
| `supportedInterfaces[0]` | The bound endpoint, JSON-RPC binding, protocol `1.0`. Always exactly one. |
| `capabilities` | `streaming: true` only. No push notifications, no extended card, no `extensions`. |
| `defaultInputModes`, `defaultOutputModes` | `text/plain` both ways. |
| `skills` | One entry per Crush skill the dispatch was given: every discovered skill when the dispatch named none. `id` and `name` are the skill name, and every entry carries the single tag `crush-skill`. |

The card declares no `securitySchemes` and no `provider`. The coordinator
never fetches the card over HTTP. It reads the same object from the
registry entry.

## Endpoints

| Request | Path | Behaviour |
| --- | --- | --- |
| `GET`, `OPTIONS` | `/.well-known/agent-card.json` | Serves the card. CORS headers reflect any `Origin`, with `Access-Control-Allow-Credentials: true`. |
| `POST` | `/` | JSON-RPC 2.0. `SendStreamingMessage` and `SubscribeToTask` answer as an SSE stream; every other method answers with one JSON response. |

The JSON-RPC handler does not check `Content-Type`, `Host` or `Origin`.

## Methods

The SDK's default request handler serves the whole A2A method set. Crush's
client calls three of them: the dispatch itself, and — when a stream
drops mid-run — the two resume methods.

| Method | Crush calls it | What the server does |
| --- | --- | --- |
| `SendStreamingMessage` | Yes, once per dispatch | Runs the turn and streams the events below. |
| `SendMessage` | No | Runs the turn and returns the final result in one response. |
| `GetTask`, `ListTasks` | `GetTask` on resume | Answer from the per-server in-memory store. |
| `CancelTask` | Yes — wander kills and user cancels | Calls the agent's `Cancel` and emits `TASK_STATE_CANCELED`, the request's reason as the status message. |
| `SubscribeToTask` | On resume | Re-attaches to a live task's stream. |
| Push-notification config methods | No | Return "push notifications not supported". |
| `GetExtendedAgentCard` | No | Returns "extended card not configured". |

### The request

The coordinator sends one user message whose only part is the dispatch
prompt. It sets no `taskId` and no `contextId`, so the server assigns both.

```json
{
  "jsonrpc": "2.0",
  "id": "5d0c…",
  "method": "SendStreamingMessage",
  "params": {
    "message": {
      "messageId": "9a41…",
      "role": "ROLE_USER",
      "parts": [{ "text": "Add table-driven tests for pkg/x…" }]
    }
  }
}
```

The executor joins every text part of the message with newlines and
ignores any other kind of part.

## Event stream

Each SSE `data:` line is a JSON-RPC response whose `result` holds exactly
one of `task`, `statusUpdate` or `artifactUpdate`. A successful dispatch
streams:

| # | Event | State | Content |
| --- | --- | --- | --- |
| 1 | `task` | `TASK_STATE_SUBMITTED` | The new task, with its IDs. |
| 2 | `statusUpdate` | `TASK_STATE_WORKING` | No message. The run has started. |
| 3 | `statusUpdate` × 0..n | `TASK_STATE_WORKING` | One per todo-list change: the current todo as message text, and the list in `metadata.todos`. |
| 4 | `artifactUpdate` × 1..n | — | The work product: the diff as chunked `text/x-diff` parts (artifact `diff`), then the typed outcome as a data part (artifact `dispatch-result`). |
| 5 | `statusUpdate` | `TASK_STATE_COMPLETED` | The agent's final text as an agent message. |

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
      "todos": [
        { "content": "Read pkg/x", "status": "completed", "active_form": "Reading pkg/x" },
        { "content": "Write tests", "status": "in_progress", "active_form": "Writing table-driven tests" }
      ]
    }
  }
}
```

- **Message text.** The first `in_progress` todo's `active_form`, else its
  `content`. With nothing in progress, the text is `N/M completed`.
- **`metadata.todos`.** Every todo, with `status` set to `pending`,
  `in_progress` or `completed`. The key is undeclared: the card lists no
  extension for it.
- **When an event is sent.** Only when the list actually changed and is
  not empty. Saves that change only usage stay silent.
- **Loss.** Delivery is lossy under back-pressure, through a 16-slot
  buffer per subscriber. A dropped snapshot is superseded by the next one.
- **Ordering.** Progress events and the terminal status are yielded from
  one goroutine, so no progress event follows a terminal one.

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
diff. One data part carries the typed outcome: `diff_bytes`,
`files_changed`, and `diff_error` when the diff could not be captured. A
diff error does not fail the run: the task still completes, and the error
rides the result.

### Other endings

| Ending | What is streamed |
| --- | --- |
| Message has no text | `TASK_STATE_REJECTED`, "message has no text to run". Crush's client refuses an empty prompt before sending, so only another client can hit this. |
| Run returns an error | `TASK_STATE_FAILED` with the error text. |
| Agent was busy, or a cancel landed at start | `TASK_STATE_FAILED`, "agent session did not start a turn (busy or canceled)". The prompt is still queued on the session, and the running agent reads it. |
| `CancelTask` | `TASK_STATE_CANCELED`, the request's reason as the status message. |

### How the client maps the outcome

| Stream outcome | `DispatchResult` |
| --- | --- |
| `TASK_STATE_COMPLETED` | `completed`. `key_findings` is the message text. `diff_summary` comes from the `diff` artifact: `(no changes)` when it is empty, `(diff unavailable: …)` when the result artifact carries a `diff_error`. |
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
  state folds and lands; a non-terminal one retries.
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

- **`contextId` and `taskId` mapping.** Each server is bound to one session
  and ignores `contextId`. Every task on a server shares that session
  ([#350](https://github.com/joestump-agent/crush/issues/350)).
- **Follow-up messages.** A second message to a busy dispatch is queued and
  run, but its task reports `TASK_STATE_FAILED`. Steering uses the
  in-process injection queue instead ([#351](https://github.com/joestump-agent/crush/issues/351)).
- **`input-required` and `auth-required`.** Dispatched agents have no
  question tool, and permission prompts use an in-process bridge
  ([#352](https://github.com/joestump-agent/crush/issues/352), [#353](https://github.com/joestump-agent/crush/issues/353)).
- **Declared extensions, usage and trace metadata** ([#359](https://github.com/joestump-agent/crush/issues/359), [#364](https://github.com/joestump-agent/crush/issues/364)).
- **Durable task state.** A restart loses every task ([#354](https://github.com/joestump-agent/crush/issues/354)).
- **Authentication** of any kind ([#357](https://github.com/joestump-agent/crush/issues/357)).

## Security model

:::danger[Unauthenticated listener]
Each dispatch's server listens on `127.0.0.1` with no authentication.

- **Who can reach it.** Any process on the machine can connect, including
  other local users on a shared host. The port is not shown in the UI,
  but that is obscurity, not protection.
- **Browsers.** The JSON-RPC handler does not check `Content-Type`, `Host`
  or `Origin`. A web page that finds the port can therefore send a
  CORS-simple `POST` that runs without a preflight.
- **What a caller can do.** `SendMessage` runs a prompt on a write-capable
  agent with the dispatch's permission policy. With yolo on, that includes
  unprompted `bash`. `CancelTask` stops the agent, and `GetTask` reads
  results.

Until the per-process 0600 unix socket lands ([#346](https://github.com/joestump-agent/crush/issues/346)), avoid dispatching
on shared machines. To turn dispatch off, run
`permissions deny dispatch_agent`.
:::

## Planned protocol surface

The 2026-10-04 re-base makes A2A carry the whole agent lifecycle. Stage 1
holds ship-blockers, stage 2 must land before
[Clustered Crush (#331)](https://github.com/joestump-agent/crush/issues/331),
and stage 3 comes after. The reasoning is in
[Design decisions](./design-decisions.md).

| Ticket | Change | Stage |
| --- | --- | --- |
| [#346](https://github.com/joestump-agent/crush/issues/346) | One A2A host per process on a 0600 unix socket; no TCP listener, JSON-only requests, no CORS reflection. | 1 |
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
