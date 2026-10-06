---
id: architecture
title: How multi-agent works today
sidebar_label: Architecture
description: The components behind dispatched agents as they exist on main — coordinator, workspace registry, the per-process A2A host and its socket client, injection queue, todo bridge, kill ladder, permission bridge — and one dispatch's lifecycle end to end.
---

# How multi-agent works today

:::info[Fork feature]
Dispatched agents, the A2A layer and everything on this page are additions
in the `joestump-agent/crush` fork.
:::

This page describes the code that ships on `main` today. That code is the
first cut of the design, landed in PRs
[#317](https://github.com/joestump-agent/crush/pull/317) to
[#339](https://github.com/joestump-agent/crush/pull/339). The review that
followed reset the target, so a lot of what you read here is scheduled to
change. Where current behaviour is known to be wrong, a **Known issue**
callout links the fix. The target design is on
[Design decisions](./design-decisions.md).

In one sentence: `dispatch_agent` provisions a git worktree, builds a
scoped agent for it, serves that agent over A2A on the process's unix
socket, and drives exactly one turn through an A2A client. Steering,
permissions, kill, progress to the UI and result delivery all happen
in-process, outside the protocol.

## The pieces

```text
 parent session (main agent)
   |  dispatch_agent(prompt, role, handle, branch, model, skills)
   v
 Coordinator ------ provision ------> Workspace + dispatch registry
   |                                   (worktree, branch, handle, status,
   | start server                       endpoint, card, result)
   v
 A2A host  http://crush-a2a/agents/<dispatch id>   one per process (unix socket)
  |   POST /agents/<dispatch id>   JSON-RPC 2.0
  v
 Executor ---- Run ----> dispatched SessionAgent (worktree toolchain)
   ^                          |
   | SSE events               | session saves (todos, usage)
   |                          v
 A2A client             Todo collector --+--> app broker --> agent block (TUI)
   |                                     +--> Executor (Working events)
   v
 DispatchResult --> hidden follow-up turn on the parent session

 Outside the protocol (direct Go calls):
   @handle / message_agent ---> injection queue ---> SessionAgent.EnqueueWhenBusy
   dispatched permission request ---> permission bridge ---> parent's prompt
   nudge ladder / watchdog ---> SessionAgent.Cancel
```

| Component | Source | Role |
| --- | --- | --- |
| Coordinator | [`internal/agent/dispatch_tool.go`](https://github.com/joestump-agent/crush/blob/main/internal/agent/dispatch_tool.go) | Implements the `dispatch_agent` tool and owns each run's lifecycle. |
| Workspace and dispatch registry | [`internal/dispatch/workspace.go`](https://github.com/joestump-agent/crush/blob/main/internal/dispatch/workspace.go) | Creates and diffs worktrees, and keeps the one in-memory registry that handles, discovery and the UI read. |
| Dispatch toolchain | [`internal/agent/dispatch.go`](https://github.com/joestump-agent/crush/blob/main/internal/agent/dispatch.go) | Builds worktree-rooted tools, scoped config, LSP and permissions, plus the permission bridge. |
| A2A server and card | [`internal/a2a/server.go`](https://github.com/joestump-agent/crush/blob/main/internal/a2a/server.go), [`agentcard.go`](https://github.com/joestump-agent/crush/blob/main/internal/a2a/agentcard.go) | One JSON-RPC host per process on a `0600` unix socket; routes `/agents/<dispatch id>` per dispatch. |
| Executor | [`internal/a2a/executor.go`](https://github.com/joestump-agent/crush/blob/main/internal/a2a/executor.go) | Maps one `SessionAgent.Run` onto A2A task states and events. |
| A2A client | [`internal/a2a/client.go`](https://github.com/joestump-agent/crush/blob/main/internal/a2a/client.go), [`internal/agent/dispatch_a2a.go`](https://github.com/joestump-agent/crush/blob/main/internal/agent/dispatch_a2a.go) | Sends the prompt and consumes the stream to a terminal state. |
| Injection queue | [`internal/agent/dispatch_inject.go`](https://github.com/joestump-agent/crush/blob/main/internal/agent/dispatch_inject.go) | Delivers mid-run messages to a running dispatch. |
| Todo collector | [`internal/dispatch/todo.go`](https://github.com/joestump-agent/crush/blob/main/internal/dispatch/todo.go) | Reduces session saves and registry events into progress snapshots. |
| Nudge ladder and wander kill | [`internal/agent/todo_enforcement.go`](https://github.com/joestump-agent/crush/blob/main/internal/agent/todo_enforcement.go), `dispatch_tool.go` | Enforces todos and cancels runaway runs. |
| Result payload | [`internal/dispatch/result.go`](https://github.com/joestump-agent/crush/blob/main/internal/dispatch/result.go) | Defines the `DispatchResult` JSON the main agent reads. |

The app wires the A2A server factory and the UI sink into the coordinator
at startup ([`internal/app/app.go`](https://github.com/joestump-agent/crush/blob/main/internal/app/app.go)).
`dispatch_agent` and `message_agent` are registered only for top-level
agents. Sub-agents and dispatched agents get neither, so delegation is one
level deep.

## Lifecycle of one dispatch

### 1. Dispatch

The main agent calls `dispatch_agent`. The tool validates the prompt and
the `model` choice (`small` by default, or `large`). It then resolves the
dispatch registry, creating it on first use. Creating the registry fails
when the working directory is not a git repository, and that failure is
cached for the life of the process.

### 2. Provision

While the tool call is still open, the coordinator runs these steps in
order. Any failure removes what was built and returns a tool error to the
model.

1. **Worktree.** It creates a worktree at
   `<cwd>/.crush/worktrees/crush-dispatch-<uuid>`, on a new branch of the
   same name. The branch is cut from the `branch` argument, else the
   current branch, else `HEAD`. Uncommitted changes in your checkout are
   not included. The base and its resolved SHA are recorded on the
   registry entry.
2. **Toolchain.** It reuses the parent's config viewed from the worktree
   path and builds the tools against it: the task agent's read-only set plus
   `bash`, `edit`, `multiedit`, `write` and `todos`. Nothing the base
   revision's config files declare is read or executed. It also builds a
   scoped LSP manager and a scoped permission service bridged to the
   parent's.
3. **Agent.** It renders the system prompt from `dispatch.md.tpl` and
   builds a `SessionAgent` on the chosen model, with the todo
   enforcement ladder resolved from the worker definition's `todos` and
   `kill` blocks layered over the global `todo_enforcement` settings
   ([#402](https://github.com/joestump-agent/crush/issues/402)).
4. **Session and handle.** It creates a task session titled "Dispatched
   Agent", a child of the parent session. The registry entry gets the
   session, the `running` status, and an `@handle` derived from `handle`,
   else `role`, else `agent`, suffixed `-2`, `-3` on collision.

:::warning[Known issue]
The model-supplied `branch` is not validated with
`--end-of-options` ([#375](https://github.com/joestump-agent/crush/issues/375)). Parallel provisions race inside git
([#381](https://github.com/joestump-agent/crush/issues/381)).
:::

### 3. Serve

The coordinator registers the dispatch on the process-wide A2A host. The
host lazily binds the unix socket `<data dir>/a2a/<pid>.sock` on its
first dispatch (socket `0600` in a `0700` directory, falling back to a
per-user temp dir when the path would overflow the socket length limit),
builds the Agent Card with the routed endpoint
`http://crush-a2a/agents/<dispatch id>`, and stamps the endpoint and card
on the registry entry. That stamp is the whole discovery mechanism: an
in-memory lookup, with no directory and no network hop. The wire format
is on [A2A protocol](./a2a-protocol.md).

**One execution path.** Every dispatch is driven through the A2A client;
there is no direct-run fallback. A host start failure fails the dispatch
with a `start A2A server: …` tool error and removes its registry entry
and workspace, so nothing runs unserved.

:::info[Authentication]
Every served call must carry the host's bearer token, declared on the
card as a `crush-bearer` `securitySchemes` entry
([#357](https://github.com/joestump-agent/crush/issues/357)). The token is
32 bytes of `crypto/rand` minted when the host binds and held only in
memory; where the platform reports socket peer credentials, the peer must
also be the same OS user. Reach is restricted to the same user by the
socket permissions, and the host rejects cross-origin, non-JSON and
wrong-`Host` requests before any dispatch work runs.
:::

### 4. Stream

The tool returns a `running` `DispatchResult` straight away, and the main
agent keeps working. The run continues on a goroutine detached from the
parent turn's context. That goroutine:

1. registers the agent with the **injection queue** for exactly the run's
   lifetime;
2. starts the **kill watchdog** when `hard_timeout` or `stall_window` is
   set;
3. drives the turn. On the transport path, the client sends the prompt as
   one `SendStreamingMessage` and reads SSE events until a terminal state.
   The executor runs the agent and emits a `Working` status for every todo
   change.

**Progress to the UI does not come from the stream.** The todo collector
subscribes once to session saves and registry transitions and reduces them
into snapshots: entry, current todo, todo counts, tokens and cost. Its one
sink is the app broker that drives the agent block. The executor
subscribes to the same collector for its `Working` events. The client
counts those events but does not re-publish them.

**Steering.** A leading `@handle` in the editor, or the main agent's
`message_agent` tool, lands in the injection queue. The queue calls
`EnqueueWhenBusy` on the running agent, so the message is folded into the
next model step or run as the immediate follow-up turn. A finished or
unknown agent refuses cleanly, because task sessions are never continuable.
This is a Go call, not an A2A message.

**Permissions.** Permission requests from the dispatched agent go to its
scoped permission service. The bridge forwards each one to the parent's
service, so it appears in the same approval prompt as the main agent's.

**Nudge ladder and wander kill.** The ladder counts tool calls without
todo activity. It injects a nudge after `nudge_threshold` calls (default
4), or right after the first mutating call, and an escalating nudge after
another full window. Once the window after the last nudge also passes
unanswered, it fires the `ignored nudges` kill. The watchdog adds
`hard timeout` and `stalled todos`, and loop detection supplies
`tool loop`. A kill records its reason and calls `SessionAgent.Cancel`.
Details are on [Todo enforcement](./todo-enforcement.md).

:::warning[Known issue]
The permission bridge is bound to the tool call's context, which ends with
the parent's turn. After that, a dispatched agent's permission prompts
block forever unless yolo is on ([#371](https://github.com/joestump-agent/crush/issues/371)).
:::

:::warning[Known issue]
The ladder's kill is wired for every agent, not just dispatched ones. With
defaults, the main agent, plan mode and `agent` sub-agents are canceled
after two ignored nudges ([#393](https://github.com/joestump-agent/crush/issues/393)). Agents without a `todos` tool are nudged anyway ([#394](https://github.com/joestump-agent/crush/issues/394)).
The stall watchdog never arms for a real dispatch, because the session
starts with no todos ([#396](https://github.com/joestump-agent/crush/issues/396)).
:::

### 5. Terminal

When the turn ends, the client maps the terminal task state onto a
`DispatchResult`. `completed` keeps the agent's final text as
`key_findings`, and the diff artifact is condensed into `diff_summary`: a
per-file stat plus the diff, cut at 250 lines. `failed`, `rejected` and
`canceled` all become `failed`. The result is built the same way for
every dispatch, and is where `killed` with a `killed_reason` comes from.

Then, in this order, the coordinator:

1. drops the injection target, so a message that arrives now is refused;
2. records the result, then the terminal status, on the registry entry,
   which renders the finished agent block;
3. adds the child session's cost to the parent session.

:::warning[Known issue]
On the transport path — the production path — kills are lost. A kill
cancels the agent behind the executor's back, the executor emits no
terminal event, and the stream hangs ([#342](https://github.com/joestump-agent/crush/issues/342)). The transport result also
ignores kill reasons, loop detection and diff errors ([#343](https://github.com/joestump-agent/crush/issues/343)).
:::

### 6. Deliver

The coordinator delivers `TerminalMessage()` to the parent session as a
**hidden follow-up turn**. That message is a review instruction plus the
result JSON. The delivery waits in a pending set outside the prompt
queue: a busy parent keeps it there while Esc, cancel, and queue clears
leave it alone, and it runs on the next idle. Results that stack up
while the parent is busy arrive in one turn, and a delivery that hits
an error is re-pended and retried. The delivery strips the dispatch
tool call's RunID, so `crush run` correlators are unaffected. The
payload stays out of the chat view, and the main agent's reply to it is
the visible outcome. Delivery is dropped, with a log line, when the
parent session no longer exists or there is no main agent. Crush never
merges: the main agent, or you, reviews the branch.

:::warning[Known issue]
In `crush run` the process exits before results arrive
([#387](https://github.com/joestump-agent/crush/issues/387)).
:::

### 7. Cleanup

When the run returns, the A2A server stops, with a 5-second graceful
shutdown. Its endpoint and card are cleared from the registry. The
toolchain then closes, which stops the permission bridge and the scoped
LSP clients. The worktree and branch stay so they can be reviewed.

When Crush exits, `App.Shutdown` releases this instance's dispatch
workspaces synchronously, bounded by a 30-second budget ([#367](https://github.com/joestump-agent/crush/issues/367)):
dispatches the human decided about — applied or dismissed — and ones that
never produced work are removed with their branches. Completed and killed
dispatches with commits or uncommitted changes stay on disk and on their
branch for salvage, and the next launch reconciles the owner markers a
crashed run left behind the same way ([#367](https://github.com/joestump-agent/crush/issues/367)). A live instance's
workspaces are never touched ([#365](https://github.com/joestump-agent/crush/issues/365)).

Shutdown itself runs before that sweep: every live dispatch is canceled
with the "crush exited" kill reason, and the exit waits (bounded) for
each dispatched run to record its terminal state, so agents stop before
messages flush and the database closes.

:::warning[Known issue]
Shutdown does not cancel running dispatches before release
([#372](https://github.com/joestump-agent/crush/issues/372)), so a dispatch still running at exit loses its workspace if
it produced no work.
:::

## Where this is going

The 2026-10-04 review made A2A the runtime contract instead of a shim
around one call. The main changes:

- one A2A host per process on a 0600 unix socket;
- no direct-run fallback;
- `contextId` is the session and `taskId` the run;
- steering is an A2A message;
- kill is `tasks/cancel` with a reason;
- a durable SQLite task store;
- agents as data, with overridable built-ins.

The full record is in [Design decisions](./design-decisions.md), the
protocol work in [Planned protocol surface](./a2a-protocol.md#planned-protocol-surface),
and the tracking epic is [#341](https://github.com/joestump-agent/crush/issues/341).
