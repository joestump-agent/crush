---
id: design-decisions
title: Design decisions
sidebar_label: Design decisions
description: The decision record for multi-agent dispatch and A2A — every dated decision, the reasoning behind the 2026-10-04 re-base, its tracking tickets, and the decisions it superseded.
---

# Design decisions

:::info[Fork feature]
Multi-agent dispatch and the A2A layer are additions in the
`joestump-agent/crush` fork.
:::

This page is the decision record for dispatched agents. It is written for
users who want to know why things work the way they do, and for
contributors picking up the work. It merges the decision logs of two design
documents:

- **The A2A coordination PRD.** This covers the protocol, transport,
  security and persistence. It is the plan behind the
  [A2A Coordination epic (#67)](https://github.com/joestump-agent/crush/issues/67).
- **The sub-agent interaction model.** This covers the agent block,
  handles, inspect mode, todo enforcement and cleanup. It is the plan
  behind the
  [Worktree Dispatch epic (#61)](https://github.com/joestump-agent/crush/issues/61).

The first implementation landed in PRs
[#317](https://github.com/joestump-agent/crush/pull/317) to
[#339](https://github.com/joestump-agent/crush/pull/339). The review of
that stack, on 2026-10-04, re-based both documents: **A2A is the runtime
contract, not a dispatch shim**. Every 2026-10-04 decision below has since
shipped; the one open follow-up is the peer registry
([#334](https://github.com/joestump-agent/crush/issues/334)), which is what
lets one Crush run a task on another. What ships today is on
[Architecture](./architecture.md); the re-base was tracked on
[#341](https://github.com/joestump-agent/crush/issues/341).

## Decision log

| Date | Decision | Source | Status |
| --- | --- | --- | --- |
| 2026-07-13 | Adopt A2A through the official a2a-go SDK. MCP stays the agent-to-tool protocol. | A2A PRD | Shipped (SDK v2.5.0) |
| 2026-07-13 | Phase 1 is in-process: one loopback A2A server per dispatch, with in-memory discovery. | A2A PRD | Shipped; superseded 2026-10-04 |
| 2026-07-13 | Dispatch progress streams to the Sidekick dashboard. | A2A PRD | Superseded 2026-10-02 |
| 2026-10-02 | Sidekick is gone. The agent block in the parent chat is the only sub-agent surface. | Both | Shipped ([#290](https://github.com/joestump-agent/crush/pull/290)) |
| 2026-10-02 | A leading `@handle` routes to the agent. Mid-sentence mentions go to the parent, with agent-card attachments. | Interaction | Shipped ([#313](https://github.com/joestump-agent/crush/issues/313)) |
| 2026-10-02 | Navigation is `ctrl+]` to enter or cycle and `ctrl+[` to go back, with Esc untouched. | Interaction | Superseded 2026-10-04 |
| 2026-10-02 | Sub-agent sessions are inspect-only and never continuable. | Interaction | Revised 2026-10-04 |
| 2026-10-02 | Mid-run message injection, handles and send-down ship in v1. | Interaction | Shipped ([#312](https://github.com/joestump-agent/crush/issues/312)) |
| 2026-10-02 | Wander kill is deterministic and configurable, and workspace state is preserved for salvage. | Interaction | Shipped ([#316](https://github.com/joestump-agent/crush/issues/316)); scope revised 2026-10-04 |
| 2026-10-04 | **A2A is the runtime contract.** One path through the A2A client, and no direct-run fallback. | Both | Shipped ([#347](https://github.com/joestump-agent/crush/issues/347)) |
| 2026-10-04 | **Transport.** One host per Crush process on a 0600 unix socket. TCP is opt-in and requires TLS and auth. | A2A PRD | Shipped: host ([#346](https://github.com/joestump-agent/crush/issues/346)), auth ([#357](https://github.com/joestump-agent/crush/issues/357)), TLS-only TCP listener with client certificates ([#358](https://github.com/joestump-agent/crush/issues/358)) |
| 2026-10-04 | **Agent definitions.** Built-ins stay backwards compatible but can be overridden, extended, or replaced by an external card. crushrc first. | Both | Shipped: data model [#333](https://github.com/joestump-agent/crush/issues/333), crushrc [#431](https://github.com/joestump-agent/crush/issues/431), runtime honors definitions [#432](https://github.com/joestump-agent/crush/issues/432), `agent` parameter [#433](https://github.com/joestump-agent/crush/issues/433), external cards for dispatch agents [#434](https://github.com/joestump-agent/crush/issues/434) |
| 2026-10-04 | **Extensions.** Metadata is allowed but must be declared on the card and statically typed. | A2A PRD | Shipped ([#359](https://github.com/joestump-agent/crush/issues/359)) |
| 2026-10-04 | **Kill is `tasks/cancel` with a reason.** Exactly one terminal event per task. | A2A PRD | Shipped ([#342](https://github.com/joestump-agent/crush/issues/342), [#348](https://github.com/joestump-agent/crush/issues/348)) |
| 2026-10-04 | **Kill scope.** Nudges apply to agents that have the todos tool. Kill applies only to dispatched agents. | Both | Shipped ([#393](https://github.com/joestump-agent/crush/issues/393), [#394](https://github.com/joestump-agent/crush/issues/394)) |
| 2026-10-04 | **Per-agent thresholds** live on the definition, with global options as defaults. | Both | Shipped ([#402](https://github.com/joestump-agent/crush/issues/402)); the `todo_enforcement` alias stays accepted |
| 2026-10-04 | **Cleanup.** Work is preserved for salvage, with explicit apply and dismiss, ownership leases, and nothing discarded at exit. | Both | Shipped: leases and exit preservation ([#365](https://github.com/joestump-agent/crush/issues/365), [#367](https://github.com/joestump-agent/crush/issues/367)), apply and dismiss tools ([#368](https://github.com/joestump-agent/crush/issues/368)), `crush dispatch` ([#369](https://github.com/joestump-agent/crush/issues/369)) |
| 2026-10-04 | **Handle lifetime is run lifetime.** Handles are scoped to the session tree. | Both | Shipped ([#399](https://github.com/joestump-agent/crush/issues/399)): released at run end, 32-byte cap, reserved names |
| 2026-10-04 | **Not continuable, for now.** Delegation depth is one level. | Both | Shipped; the `--session` and `run --continue` gaps closed in [#413](https://github.com/joestump-agent/crush/issues/413) |
| 2026-10-04 | **Navigation.** `ctrl+]` enters and cycles. Esc and `ctrl+[` always leave inspect mode, and Esc never cancels from it. | Interaction | Shipped ([#404](https://github.com/joestump-agent/crush/issues/404)) |
| 2026-10-04 | **Durable task state** in a SQLite task store. | A2A PRD | Shipped: the store ([#354](https://github.com/joestump-agent/crush/issues/354)) and startup reconcile ([#355](https://github.com/joestump-agent/crush/issues/355)) |
| 2026-10-04 | **Steering, kill and progress are A2A operations.** | Interaction | Shipped: typed progress ([#359](https://github.com/joestump-agent/crush/issues/359)), steering ([#351](https://github.com/joestump-agent/crush/issues/351)), kill ([#348](https://github.com/joestump-agent/crush/issues/348)), the TUI as an A2A client ([#421](https://github.com/joestump-agent/crush/issues/421)) |

## The 2026-10-04 decisions

### A2A is the runtime contract

The first cut put a protocol boundary around one call: the dispatch prompt
and its terminal result. Everything else crossed through shared memory. The
direct path and the A2A path then diverged on kill reasons, loop
attribution and diff errors, and the SDK's 3-minute client timeout failed
every long dispatch. With a single path through the A2A client — the path
that ships — kill, timeout and steering cannot diverge. Process isolation
([#72](https://github.com/joestump-agent/crush/issues/72)), remote workers
([#73](https://github.com/joestump-agent/crush/issues/73)) and third-party
agents ([#74](https://github.com/joestump-agent/crush/issues/74)) become a
choice of transport rather than a re-plumb.
Tracking: [#347](https://github.com/joestump-agent/crush/issues/347), [#343](https://github.com/joestump-agent/crush/issues/343), [#392](https://github.com/joestump-agent/crush/issues/392).

### One host per process, on a unix socket

Each dispatch used to open an unauthenticated JSON-RPC listener on
`127.0.0.1`, which any local process, or a web page, could use to drive a
write-capable agent. Now one host per process listens on a 0600 unix
socket in a 0700 directory, so the operating system puts access control
in place and no network listener opens at all, and middleware rejects
cross-origin, non-JSON and wrong-`Host` requests before any dispatch
work runs. One host per process gives one place to route, authenticate
and observe. TCP remains available for remote and third-party agents,
but plain TCP is refused: it needs TLS, auth interceptors and
`securitySchemes` on the card.
Tracking: shipped in [#346](https://github.com/joestump-agent/crush/issues/346), [#357](https://github.com/joestump-agent/crush/issues/357) and [#358](https://github.com/joestump-agent/crush/issues/358).

### Agents are definitions

Before the re-base a dispatched agent was the hard-coded `task` preset with
write tools forced on. The only model choice was large or small, and agent
config could not be loaded at all. Making every agent a definition — an
Agent Card plus a runtime spec — lets users override built-ins field by
field and add their own. The built-ins are `coder`, `plan`, `task`, and a new `worker` for
dispatch. A `runtime: a2a` definition swaps in an external agent behind the
same client, and its artifacts are always treated as untrusted
([#434](https://github.com/joestump-agent/crush/issues/434): its credential
is pinned to the card's origin, its output is marked untrusted, and its input
and permission requests are refused). crushrc is
the primary format, with JSON kept for compatibility. The `crush.json` data
model and its validation landed in
[#333](https://github.com/joestump-agent/crush/issues/333); the runtime has
honored definitions since #432, and the host publishes one card per
definition.
Tracking: [#333](https://github.com/joestump-agent/crush/issues/333), [#431](https://github.com/joestump-agent/crush/issues/431), [#432](https://github.com/joestump-agent/crush/issues/432), [#433](https://github.com/joestump-agent/crush/issues/433), [#434](https://github.com/joestump-agent/crush/issues/434).

### Extensions are declared and statically typed

The A2A epic originally ruled out custom extensions, yet todo progress
already traveled in an undeclared metadata key. Instead of pretending
otherwise, each kind of Crush metadata gets an extension URI, a Go type and
a JSON schema. Cards list what they emit and accept, and undeclared keys
are dropped. Two extensions ship today: `todos/v1` carries dispatch todo
progress ([#359](https://github.com/joestump-agent/crush/issues/359)), and
`usage/v1` carries the child session's token totals, cost, model and trace
id on every post-run terminal status
([#364](https://github.com/joestump-agent/crush/issues/364)). The planned
extension is `delegation/v1`, carrying origin, chain, depth and budget; it
subsumes [#332](https://github.com/joestump-agent/crush/issues/332) and
[#336](https://github.com/joestump-agent/crush/issues/336).

Tracking: [#364](https://github.com/joestump-agent/crush/issues/364).

### Kill is `tasks/cancel` with a reason

Kills used to cancel the agent behind the executor's back. The executor
then emitted no terminal event and the SDK stream hung, which a probe
confirmed. Every kill now goes through `CancelTask` with a reason: wander
kill, user cancel and parent shutdown. The executor guarantees exactly one
terminal event per task: `Completed`, `Failed` or `Canceled{reason}`. That
way the kill reason reaches the parent on every transport, and the SDK's
inactivity timeout (`inactivity_timeout`) catches whatever the in-process
watchdogs miss.
Tracking: [#342](https://github.com/joestump-agent/crush/issues/342), [#348](https://github.com/joestump-agent/crush/issues/348), [#345](https://github.com/joestump-agent/crush/issues/345), [#360](https://github.com/joestump-agent/crush/issues/360).

### Kill scope and per-agent thresholds

A kill is only safe where something can recover from it. A dispatched
agent's parent can re-dispatch, but killing the main agent or plan mode
just cancels your turn. So nudges apply to every agent that has the todos
tool, agents without it are not nudged, and the kill applies only to
dispatched agents. Thresholds live on each agent definition, with the
global options as defaults, settable from crushrc.
Tracking: [#393](https://github.com/joestump-agent/crush/issues/393), [#394](https://github.com/joestump-agent/crush/issues/394), [#402](https://github.com/joestump-agent/crush/issues/402), [#403](https://github.com/joestump-agent/crush/issues/403).

### Preserve work, clean up explicitly

The original exit sweep force-removed every dispatch worktree. That
included completed work nobody reviewed, killed work the parent was just
told is preserved, and, as reproduced in review, another Crush instance's
live worktrees. Under the new rule nothing a human might want is discarded
automatically. Work leaves only through `apply_dispatch`,
`dismiss_dispatch` or `crush dispatch prune`. Each workspace carries an
ownership lease, so instances never reap each other, and startup
reconciliation handles crashes.
Tracking: [#365](https://github.com/joestump-agent/crush/issues/365), [#367](https://github.com/joestump-agent/crush/issues/367), [#368](https://github.com/joestump-agent/crush/issues/368), [#369](https://github.com/joestump-agent/crush/issues/369).

### Handle lifetime is run lifetime

Handles used to be held until the exit sweep, so suffixes piled up
(`tester-2`, `tester-3`) and any session's model could steer another
session's agent. Releasing a handle when its run ends, and scoping handles
to the dispatching session's tree, keeps `@handle` meaning "the live agent
I am working with". A finished agent's block and card keep the handle they
were given; the name itself is free for the next dispatch.
Tracking: [#399](https://github.com/joestump-agent/crush/issues/399).

### Not continuable; one level of delegation

A task session records one dispatch's run, and its result has already been
delivered, so continuing it would contradict the record. No picker,
`--session` or `run --continue` path can make one active, and a finished
agent refuses messages with "dispatch a new agent". Delegation
stays one level deep. Larger topologies come from clustering Crush
instances ([#331](https://github.com/joestump-agent/crush/issues/331)),
not from recursion inside one session.
Tracking: [#413](https://github.com/joestump-agent/crush/issues/413).

### Esc always leaves inspect mode

The original rule left Esc alone and used `ctrl+[` to go back. On
terminals without the kitty keyboard protocol, though, `ctrl+[` *is* Esc
(`0x1b`), so the rule could not hold. On terminals that do tell them apart,
two Esc presses in inspect mode canceled the parent's run. Now both keys
leave inspect mode on every terminal. Esc never cancels from inspect mode;
you cancel from the parent view.
Tracking: [#404](https://github.com/joestump-agent/crush/issues/404).

### Durable task state

Served tasks live in the SQLite task store ([#354](https://github.com/joestump-agent/crush/issues/354)),
and every dispatch leaves one durable record ([#355](https://github.com/joestump-agent/crush/issues/355)):
a row in `a2a_dispatches` written at start, updated with the terminal
result, and stamped delivered when the parent's delivery turn succeeds.
On startup, before the UI loads any session, reconcile fails every task
and dispatch record whose owning process died — the dispatch error names
the preserved workspace — and re-delivers every terminal result the
parent never received, stamping it on the parent's persisted
`dispatch_agent` tool result as it goes. That stamp is the terminal
record a reloaded agent block renders, in process or against a server,
instead of a forever-working stale handle
([#410](https://github.com/joestump-agent/crush/issues/410), [#421](https://github.com/joestump-agent/crush/issues/421)).

Tracking: [#354](https://github.com/joestump-agent/crush/issues/354), [#355](https://github.com/joestump-agent/crush/issues/355), [#410](https://github.com/joestump-agent/crush/issues/410).

### Steering, kill and progress are A2A operations

In the first cut `@handle` and `message_agent` called a Go method, kills
called `agent.Cancel`, and progress reached the UI through an in-process
collector. None of those could cross a process boundary. As A2A operations
they are a message on the running context, `tasks/cancel` and typed
`Working` events. The same code then serves a local worker, an isolated
process and a remote agent, and the TUI is one more A2A client.
Tracking: typed progress — statically typed todo metadata under a declared extension — shipped in [#359](https://github.com/joestump-agent/crush/issues/359); steering — a mid-run message as a new A2A task on the running context, terminal on the queue's consumption verdict — in [#351](https://github.com/joestump-agent/crush/issues/351); the kill in [#348](https://github.com/joestump-agent/crush/issues/348); the TUI as an A2A client, in process and through the server, in [#421](https://github.com/joestump-agent/crush/issues/421).

## Standing implementation choices

These came out of the first implementation and still hold. Each row notes
what the re-base changes.

| Choice | Re-base |
| --- | --- |
| No automated merge. The main agent, or you, reviews the branch. | Kept. `apply_dispatch` merges only when asked ([#368](https://github.com/joestump-agent/crush/issues/368)). |
| One dispatch is one ephemeral task session, a child of the parent session. | Kept; `contextId` becomes that session ([#350](https://github.com/joestump-agent/crush/issues/350)). |
| The workspace is SCM-agnostic, with git worktrees as the first backend. | A `WorkspaceProvider` under `<data dir>/worktrees/<repo-key>/`, separate from the agent registry ([#391](https://github.com/joestump-agent/crush/issues/391), [#383](https://github.com/joestump-agent/crush/issues/383)). |
| Dispatched agents use the small model by default. | The dispatch agent's definition decides; the `worker` says small ([#433](https://github.com/joestump-agent/crush/issues/433)). |
| Dispatched tools are the read-only preset plus write tools; no MCP, sub-agents, questions or semantic search. | Comes from the definition (`@read`, `@write`, job and diagnostics tools, MCP via `mcp.allow`), with deny lists, hooks and live yolo applied on top ([#376](https://github.com/joestump-agent/crush/issues/376), [#377](https://github.com/joestump-agent/crush/issues/377), [#378](https://github.com/joestump-agent/crush/issues/378), [#384](https://github.com/joestump-agent/crush/issues/384)). Questions are back for interactive parents, as `input-required` pauses on the A2A task ([#352](https://github.com/joestump-agent/crush/issues/352)). |
| Results arrive as a hidden follow-up turn on the parent. | Redelivered until consumed, and batched ([#388](https://github.com/joestump-agent/crush/issues/388)). |
| The diff summary is capped at 250 lines. | Kept, plus a 32 KiB byte budget and a 100-file stat cap; the full patch ships as a chunked artifact ([#361](https://github.com/joestump-agent/crush/issues/361)). |

## Superseded decisions

| Was | Decided | Replaced by |
| --- | --- | --- |
| **Sidekick dashboard.** Progress streams to a Sidekick panel via `SidekickUpdate`. | 2026-07-13 | 2026-10-02: Sidekick was reverted ([#290](https://github.com/joestump-agent/crush/pull/290)). The agent block in the parent chat is the only surface, fed by the same todo snapshots. |
| **No pre-defined agent definitions.** Sub-agent prompts are rendered at dispatch time from nothing but the template. | 2026-10-02 | 2026-10-04: agents are definitions. Prompts are still rendered at dispatch time, from the definition's template. |
| **Esc untouched.** Only `ctrl+[` leaves inspect mode. | 2026-10-02 | 2026-10-04: Esc and `ctrl+[` both leave inspect mode, and Esc never cancels from it ([#404](https://github.com/joestump-agent/crush/issues/404)). |
| **Loopback TCP for Phase 1.** One `127.0.0.1` server per dispatch; gRPC and auth wait for remote workers. | 2026-07-13 | 2026-10-04: one host per process on a 0600 unix socket; TCP only with TLS and auth ([#346](https://github.com/joestump-agent/crush/issues/346), [#358](https://github.com/joestump-agent/crush/issues/358), [#357](https://github.com/joestump-agent/crush/issues/357)). |
| **Session-end sweep.** Every dispatch worktree is removed on session end, even an abandoned one ([#63](https://github.com/joestump-agent/crush/issues/63)). | Original plan | 2026-10-05: nothing is discarded at exit. Only decided and workless workspaces are released; work is kept for salvage; ownership leases, with explicit apply and dismiss ([#365](https://github.com/joestump-agent/crush/issues/365), [#367](https://github.com/joestump-agent/crush/issues/367), [#368](https://github.com/joestump-agent/crush/issues/368)). |
| **No custom extensions.** Use the A2A spec as-is. | 2026-07-13 | 2026-10-04: declared, statically typed extensions ([#359](https://github.com/joestump-agent/crush/issues/359)). |
| **Fallback over failure.** A server that fails to start is logged and the dispatch runs directly. | Phase 1 code | 2026-10-04: a host start failure is a dispatch failure ([#347](https://github.com/joestump-agent/crush/issues/347)). |
| **Steering in a later phase.** Mid-run messages wait for the A2A transport. | 2026-07-13 | 2026-10-02: injection shipped in v1 as the seam A2A will use ([#312](https://github.com/joestump-agent/crush/issues/312)). |
