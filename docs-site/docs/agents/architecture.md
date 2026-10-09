---
id: architecture
title: How multi-agent works
sidebar_label: Architecture
description: The components behind dispatched agents as they ship on main — coordinator, agent definitions, workspace provider and registry, the per-process A2A host, its SQLite task store, the injection queue, todo bridge, kill path and permission prompts — and one dispatch's lifecycle end to end.
---

# How multi-agent works

:::info[Fork feature]
Dispatched agents, the A2A layer and everything on this page are additions
in the `joestump-agent/crush` fork.
:::

This page describes the code that ships on `main`. The first cut landed in
PRs [#317](https://github.com/joestump-agent/crush/pull/317) to
[#339](https://github.com/joestump-agent/crush/pull/339); the 2026-10-04
review re-based it on A2A as the runtime contract, and that re-base has
shipped. The reasoning is on [Design decisions](./design-decisions.md).

In one sentence: `dispatch_agent` resolves an agent definition, provisions a
git worktree, builds a scoped agent for it, serves that agent over A2A on the
process's host, and drives exactly one turn through an A2A client. A steer is
an A2A message on the running context; a question or permission request
parks the task in `input-required` until the answer arrives on the same
task; a kill is a `tasks/cancel` carrying its reason; and the TUI follows,
steers and cancels agents through the host's agent index and the protocol.

## The pieces

```text
 parent session (main agent)
   |  dispatch_agent(prompt, agent, role, handle, branch, model, skills)
   |  message_agent · cancel_dispatch · apply_dispatch · dismiss_dispatch
   v
 Coordinator ---- resolve definition ----> config.Agents (coder, plan, task, worker, …)
   |  provision                                   |
   v                                              v
 WorkspaceProvider (git worktree)      Dispatch registry (handle, status,
   <data dir>/worktrees/<repo-key>/     endpoint, card, result, lease)
   |
   | serve
   v
 A2A host — one per process, unix socket (+ optional TLS on TCP)
   /agents/<definition id>   one route per definition (card built, run rejected)
   /agents/<dispatch id>     route for one running dispatch
   /agents                   index of running dispatches (socket only)
   |   POST  JSON-RPC 2.0
   v
 Executor ---- Run ----> dispatched SessionAgent (worktree toolchain)
   ^   |                       |
   |   | SQLite task store     | session saves (todos, usage)
   |   v                       v
 A2A client             Todo collector -----> executor Working events
   |                          |                   + agent block
   v
 DispatchResult --> a2a_dispatches record --> hidden follow-up turn

 steer (@handle, message_agent) --> SendStreamingMessage on the context
 kill (ladder, watchdog, ctrl+x, exit) --> tasks/cancel {reason}
 question / permission --> input-required --> answer on the same task
```

| Component | Source | Role |
| --- | --- | --- |
| Coordinator and dispatch tool | [`internal/agent/dispatch_tool.go`](https://github.com/joestump-agent/crush/blob/main/internal/agent/dispatch_tool.go) | Implements `dispatch_agent`, the concurrency cap, and owns each run's lifecycle. |
| Decision tools | [`dispatch_cancel.go`](https://github.com/joestump-agent/crush/blob/main/internal/agent/dispatch_cancel.go), [`dispatch_apply.go`](https://github.com/joestump-agent/crush/blob/main/internal/agent/dispatch_apply.go) | `cancel_dispatch`, `apply_dispatch`, `dismiss_dispatch`. |
| Agent definitions | [`internal/config/agent_definition.go`](https://github.com/joestump-agent/crush/blob/main/internal/config/agent_definition.go), [`internal/shellconfig/agent.go`](https://github.com/joestump-agent/crush/blob/main/internal/shellconfig/agent.go) | The `agents` block and the `agent` builtin: built-ins, overlays, validation, the `@read`/`@write` groups. |
| Workspace provider | [`internal/dispatch/provider.go`](https://github.com/joestump-agent/crush/blob/main/internal/dispatch/provider.go) | Creates, leases and diffs git worktrees under `<data dir>/worktrees/<repo-key>/`. |
| Dispatch registry | [`internal/dispatch/registry.go`](https://github.com/joestump-agent/crush/blob/main/internal/dispatch/registry.go) | The in-memory registry of dispatches: handles, status, endpoint, card, result. |
| Dispatch toolchain | [`internal/agent/dispatch.go`](https://github.com/joestump-agent/crush/blob/main/internal/agent/dispatch.go) | Builds worktree-rooted tools from the definition, scoped config, LSP, permissions, hooks. |
| A2A host and cards | [`internal/a2a/server.go`](https://github.com/joestump-agent/crush/blob/main/internal/a2a/server.go), [`agentcard.go`](https://github.com/joestump-agent/crush/blob/main/internal/a2a/agentcard.go), [`tcp.go`](https://github.com/joestump-agent/crush/blob/main/internal/a2a/tcp.go) | One JSON-RPC host per process on a `0600` unix socket, with the opt-in TLS listener; routes per definition and per dispatch. |
| Task store | [`internal/a2a/taskstore.go`](https://github.com/joestump-agent/crush/blob/main/internal/a2a/taskstore.go), [`reconcile.go`](https://github.com/joestump-agent/crush/blob/main/internal/a2a/reconcile.go) | The SQLite task store (`a2a_tasks`) and the durable dispatch records (`a2a_dispatches`) reconciled at startup. |
| Executor | [`internal/a2a/executor.go`](https://github.com/joestump-agent/crush/blob/main/internal/a2a/executor.go) | Maps one `SessionAgent.Run` onto A2A task states and events; parks questions and permissions. |
| A2A client | [`internal/a2a/client.go`](https://github.com/joestump-agent/crush/blob/main/internal/a2a/client.go), [`internal/agent/dispatch_a2a.go`](https://github.com/joestump-agent/crush/blob/main/internal/agent/dispatch_a2a.go) | Sends the prompt, answers and steers; consumes the stream to a terminal state; resumes a dropped stream. |
| External agents | [`internal/a2a/external.go`](https://github.com/joestump-agent/crush/blob/main/internal/a2a/external.go), [`internal/agent/dispatch_external.go`](https://github.com/joestump-agent/crush/blob/main/internal/agent/dispatch_external.go) | Fetches and pins a third-party card, runs the dispatch against it, scrubs and labels its output. |
| Injection queue | [`internal/agent/dispatch_inject.go`](https://github.com/joestump-agent/crush/blob/main/internal/agent/dispatch_inject.go) | `message_agent` and the `@handle` front door for steers. |
| Todo collector | [`internal/dispatch/todo.go`](https://github.com/joestump-agent/crush/blob/main/internal/dispatch/todo.go) | Reduces session saves and registry events into progress snapshots for the executor and the agent block. |
| Agent surface | [`internal/a2a/agents_index.go`](https://github.com/joestump-agent/crush/blob/main/internal/a2a/agents_index.go), [`internal/workspace/agent_task_watcher.go`](https://github.com/joestump-agent/crush/blob/main/internal/workspace/agent_task_watcher.go) | Lists the host's dispatches and follows their tasks; the TUI reads, steers and cancels through it, in process or through the server. |
| Nudge ladder and wander kill | [`internal/agent/todo_enforcement.go`](https://github.com/joestump-agent/crush/blob/main/internal/agent/todo_enforcement.go), `dispatch_tool.go` | Enforces todos and kills runaway dispatched runs. |
| Result payload | [`internal/dispatch/result.go`](https://github.com/joestump-agent/crush/blob/main/internal/dispatch/result.go) | Defines the `DispatchResult` JSON the main agent reads. |
| Shell | [`internal/cmd/dispatch.go`](https://github.com/joestump-agent/crush/blob/main/internal/cmd/dispatch.go) | `crush dispatch list` and `prune`. |

The app wires the A2A host and the SQLite task store into the coordinator at
startup ([`internal/app/app.go`](https://github.com/joestump-agent/crush/blob/main/internal/app/app.go)).
The coordinator publishes one card per enabled agent definition on the host
as it starts, so the host and its optional TCP listener are listening before
any dispatch. The cards live in the host's listing; the router answers `404`
for every sub-path of a route, so the well-known card path each route mounts
is not reachable over HTTP yet. The five dispatch tools are registered only for the top-level
agent in an interactive session; sub-agents and dispatched agents get none,
so delegation is one level deep, and `crush run` offers none either.

## Lifecycle of one dispatch

### 1. Dispatch

The main agent calls `dispatch_agent`. The tool reserves a concurrency slot —
at the cap it refuses with `dispatch at capacity` before touching anything —
then resolves the agent definition: the `agent` argument, else
`options.dispatch.default_agent`. The definition must be an enabled `dispatch`
agent that is not unusable; a pinned definition refuses the call's `model`
argument. It then resolves the dispatch registry, creating it on first use.
Creating the registry fails when the working directory is not a git
repository, and that failure is cached for the life of the process. A
`runtime: a2a` definition takes the [external path](#external-agents) from
here.

### 2. Provision

While the tool call is still open, the coordinator runs these steps in
order. Any failure removes what was built and returns a tool error to the
model.

1. **Worktree.** The workspace provider creates a worktree at
   `<data dir>/worktrees/<repo-key>/crush-dispatch-<uuid>`, on a new branch
   of the same name, and takes an ownership lease on it. The branch is cut
   from the `branch` argument (passed to git after `--end-of-options`), else
   the current branch, else `HEAD`. Uncommitted changes in your checkout are
   not included. The base and its resolved SHA are recorded on the registry
   entry.
2. **Toolchain.** It reuses the parent's config viewed from the worktree
   path and builds the tools against it: the fixed worktree-rooted set,
   filtered by the definition's allow list and your deny list, plus the
   definition's MCP tools and `question` when the parent is interactive.
   Nothing the base revision's config files declare is read or executed. It
   also builds a scoped LSP manager, a scoped permission service that
   follows the parent's live yolo state and allowed tools, the parent's
   `PreToolUse` hooks, and — for `question` — the dispatch's own question
   service.
3. **Agent.** It renders the system prompt from the definition
   (`builtin:dispatch` unless it names a file), appends `prompt_append`, and
   builds a `SessionAgent` on the definition's model, with the todo
   enforcement ladder resolved from the definition's `todos` and `kill`
   blocks layered over the global settings, and the kill rung wired.
4. **Session and handle.** It creates a task session titled "Dispatched
   Agent", a child of the parent session, writes the durable
   `a2a_dispatches` record, and gives the registry entry the session, the
   `running` status, and an `@handle` derived from `handle`, else `role`,
   else `agent`, suffixed `-2`, `-3` on collision with a live or reserved
   handle.

### 3. Serve

The coordinator registers the dispatch on the process-wide A2A host, which
has been listening since startup on `<data dir>/a2a/<pid>.sock` (socket
`0600` in a `0700` directory, or a per-user temp directory when the path
would overflow the socket length limit). The route `/agents/<dispatch id>`
gets its Agent Card, stamped on the registry entry — which is what the
coordinator and the agent index read; the card is not fetched over HTTP — and
the dispatch's context — its task session — is bound to the route for exactly
the run's lifetime. The wire format is on
[A2A protocol](./a2a-protocol.md).

**One execution path.** Every dispatch is driven through the A2A client;
there is no direct-run fallback. A host start failure fails the dispatch
with a `start A2A server: …` tool error and removes its registry entry
and workspace, so nothing runs unserved.

:::info[Authentication]
Every served call must carry the host's bearer token, declared on the
card as a `crush-bearer` `securitySchemes` entry. The token is 32 bytes of
`crypto/rand` minted when the host binds and held only in memory; where the
platform reports socket peer credentials, the peer must also be the same OS
user. Reach is restricted to the same user by the socket permissions, and the
host rejects cross-origin, non-JSON and wrong-`Host` requests before any
dispatch work runs. The TCP listener adds client-certificate authentication
on top; see [Working with other agents](./team.md).
:::

### 4. Stream

The tool returns a `running` `DispatchResult` straight away, and the main
agent keeps working. The run continues on a goroutine detached from the
parent turn's context. That goroutine:

1. registers the agent with the **injection queue** for exactly the run's
   lifetime;
2. starts the **kill watchdog** when `hard_timeout` or `stall_window` is
   set;
3. drives the turn. The client sends the prompt as one
   `SendStreamingMessage` and reads SSE events until a terminal state. The
   executor runs the agent and emits a `Working` status for every todo
   change, persisting each task event to the SQLite store.

**Progress to the UI does not come from the stream.** The todo collector
subscribes once to session saves and registry transitions and reduces them
into snapshots: entry, current todo, todo counts, tokens and cost. Its one
sink is the app broker that drives the agent block. The executor
subscribes to the same collector for its `Working` events. The client
counts those events but does not re-publish them.

**Steering.** A leading `@handle` in the editor, or the main agent's
`message_agent` tool, lands in the injection queue, which sends it as a
message on the dispatch's running context. The executor hands it to the
running agent, so the message is folded into the next model step or run as
the immediate follow-up turn, and the steer's own task completes once the
agent has consumed it. A finished or unknown agent refuses cleanly, because
task sessions are never continuable; an external agent refuses because A2A
gives it no such contract.

**Questions.** The dispatched agent's `question` tool asks through the
dispatch's own question service. The executor watches it: a question
parks the run — the agent stays blocked in the tool — and ends the
stream with `input-required`, the question as a typed data part. The
client hands it to the coordinator, which shows it in the parent's
question prompt labeled with the `@handle`, or answers "no interactive
user; proceed with your best judgment" when no one is there. The answer
goes back as a message on the same task, and the same run carries on.
`hard_timeout` keeps counting while a question waits, and a kill cancels
the parked task. The wire details are on
[A2A protocol](./a2a-protocol.md#questions).

**Permissions.** Permission requests from the dispatched agent go to its
scoped permission service. One that needs a person parks the run in
`input-required` on the A2A task, and the parent's transport puts it
through the parent's own service, so it appears in the same approval
prompt as the main agent's. See
[Permission prompts](./a2a-protocol.md#permission-prompts).

**Nudge ladder and wander kill.** The ladder counts tool calls without
todo activity. It injects a nudge after `nudge_threshold` calls (default
4), or right after the first mutating call, and escalating nudges on the
following windows up to `kill_after_nudges`. Once the window after the last
allowed nudge also passes unanswered, it fires the `ignored nudges` kill. The
watchdog adds `hard timeout` and `stalled todos`, loop detection supplies
`tool loop`, and <kbd>ctrl+x</kbd> or `cancel_dispatch` supplies
`canceled by user`. Every kill records its reason on the run's kill state and
sends one `tasks/cancel` carrying it; the executor emits exactly one terminal
`canceled` event with the reason, and a direct cancel remains the fallback
for a kill that lands before the stream has named its task. The ladder's kill
rung is wired only for dispatched agents; every other agent tops out at the
escalating nudge. Details are on [Todo enforcement](./todo-enforcement.md).

### 5. Terminal

When the turn ends, the client maps the terminal task state onto a
`DispatchResult`. `completed` keeps the agent's final text as
`key_findings`, and the diff artifact is condensed into `diff_summary`: a
per-file stat plus the diff, cut at 250 lines. A `canceled` status whose
message is a kill reason becomes `killed` with that `killed_reason`;
`failed` and `rejected` become `failed`. Steers the agent accepted but never
read are listed as `undelivered_steers`.

Then, in this order, the coordinator:

1. drops the injection target, so a message that arrives now is refused;
2. records the result, then the terminal status, on the registry entry and
   the `a2a_dispatches` record, which renders the finished agent block and
   releases the handle;
3. adds the child session's cost to the parent session.

### 6. Deliver

The coordinator delivers `TerminalMessage()` to the parent session as a
**hidden follow-up turn**. That message is a review instruction plus the
result JSON. The delivery waits in a pending set outside the prompt
queue: a busy parent keeps it there while Esc, cancel, and queue clears
leave it alone, and it runs on the next idle. Results that stack up
while the parent is busy arrive in one turn, and a delivery that hits
an error is re-pended and retried. Once delivered, the result is stamped on
the parent's persisted `dispatch_agent` tool result — the record a reloaded
agent block renders — and the dispatch record is marked delivered. Delivery
is dropped, with a log line, when the parent session no longer exists or
there is no main agent. Crush never merges on its own: the main agent, or
you, reviews the branch and settles it with `apply_dispatch` or
`dismiss_dispatch`.

### 7. Cleanup

When the run returns, the dispatch's route stops, with a 5-second graceful
shutdown, and its endpoint and card are cleared from the registry. The
toolchain then closes, which stops the scoped LSP clients. The worktree and
branch stay so they can be reviewed; `apply_dispatch` and `dismiss_dispatch`
remove them and record the disposition on the workspace's owner marker.

When Crush exits, shutdown first cancels every live dispatch with the
`crush exited` kill reason and waits (bounded) for each run to record its
terminal state, so agents stop before messages flush and the database
closes. Then `App.Shutdown` releases this instance's dispatch workspaces
synchronously, bounded by a 30-second budget: dispatches the human decided
about — applied or dismissed — and ones that never produced work are removed
with their branches. Completed and killed dispatches with commits or
uncommitted changes stay on disk and on their branch for salvage. A live
instance's workspaces are never touched, and at the next launch the
reconciler fails every task and dispatch record whose owning process died,
re-delivers every terminal result the parent never received, and handles
the owner markers a crashed run left behind the same way. From the shell,
`crush dispatch list` and `crush dispatch prune` read the same markers.

## External agents

A `runtime: a2a` definition skips provisioning: there is no worktree,
toolchain or local agent. The coordinator resolves the definition's
`auth.token`, fetches and validates the card (origin-pinned, no credential
on the fetch), and sends the prompt as a new task to the card's JSON-RPC
interface. The dispatch still gets a registry entry, handle, role, task
session and durable record, so the agent block, `@handle` card and
`cancel_dispatch` work; steering, `apply_dispatch` and `dismiss_dispatch`
refuse. The stream is bounded by `transport.idle_timeout` and `kill.timeout`,
its text is scrubbed and labeled untrusted, and its input and auth requests
are answered with a cancel. The rules are under
[External agents](./configuration.md#external-agents).

## Other entry points

The same host serves more than dispatches. The `agentic_fetch` sub-agent
runs its turn over A2A on a route of its own, and every agent definition —
`coder`, `plan`, `task`, `worker` and yours — has a card in the host's
listing and a route at `/agents/<id>`. Those definition routes carry no run
today: a message on one is rejected with `no running agent for context …`
until an entry point serves a turn on it, and the card is not served on the
wire. The `agent` tool's sub-agent, plan mode and the main
coder still run in process. Remote peers are authenticated by the TCP
listener but reach no local run until the peer registry
([#334](https://github.com/joestump-agent/crush/issues/334)).

## What changed in the re-base

The 2026-10-04 review made A2A the runtime contract instead of a shim
around one call. Everything below has shipped:

- one A2A host per process on a 0600 unix socket, with an opt-in TLS
  listener on TCP;
- no direct-run fallback;
- `contextId` is the session and `taskId` the run;
- steering is an A2A message, questions and permissions are
  `input-required` pauses, and a kill is `tasks/cancel` with a reason;
- a durable SQLite task store with startup reconciliation and result
  re-delivery;
- agents as data: overridable built-ins, user-defined dispatch agents, and
  external agents behind a card;
- explicit `apply_dispatch` / `dismiss_dispatch`, ownership leases, and
  nothing discarded at exit;
- handles released at run end, with a length cap and reserved names;
- <kbd>esc</kbd> and <kbd>ctrl+[</kbd> both leave inspect mode, and
  <kbd>esc</kbd> never cancels from it.

The full record is in [Design decisions](./design-decisions.md).
