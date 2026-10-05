---
id: handles-and-inspect
title: Handles and inspect mode
sidebar_label: Handles and inspect
sidebar_position: 3
description: Reference for @handles, agent cards, the @ completions, message_agent, the agent block, inspect mode, and the sub-agent sessions tree.
---

# Handles and inspect mode

:::info[Fork feature]
@handles, agent cards, inspect mode, and the sessions tree are additions in the
`joestump-agent/crush` fork.
:::

This is the reference for every surface that shows or reaches a
[dispatched agent](/agents/overview). For a guided tour, see
[Your first parallel task](/agents/tutorial).

## Handles

A dispatched agent's handle is the `handle` the main agent passed to
`dispatch_agent`, else a slug of its `role`, else `agent`. Slugging drops a
leading `@`, lower-cases, collapses each run of characters other than `a-z` and
`0-9` into one `-`, and trims dashes: `@Team Lead` becomes `team-lead`.

Handles are unique across the Crush process, not per session: a second `tester`
becomes `tester-2`, then `tester-3`. Assignment is atomic, so agents dispatched
together never collide. A handle stays reserved until Crush exits, even after
its agent finishes.

The handle shows in the agent block's header, the @ completions, the dispatch
result's `handle` field, and as the name on the agent's A2A card.

:::info[Planned]
A handle will be released when its run ends. Lookups will be scoped to the
caller's session tree, and handles will get a length cap and reserved names
([#399](https://github.com/joestump-agent/crush/issues/399)).
:::

## Leading @handle: steering

A message whose first token is a handle goes to that agent, not the main agent:

```text
@tester skip the benchmarks; just the table tests
```

The handle must open the first line (leading spaces are ignored) and be
followed by whitespace or the end of the message. Handle tokens are letters,
digits, `-`, and `_`, matched case-insensitively.

| You send | What happens |
| --- | --- |
| A running agent's handle, then text | Queued for that agent. Your message and its reply show on its block; nothing lands in the main conversation. |
| A finished agent's handle, then text | Refused: `agent @tester finished (completed); task sessions are never continuable — dispatch a new agent instead`. The message is consumed. |
| A handle-shaped token alone | Warning: `Nothing to send @handle — …`. The message is consumed. |
| Anything else | An ordinary prompt to the main agent. |

A queued message is folded into the agent's turn at its next model step, or
runs as its immediate follow-up turn, in the order you sent it.

:::warning[Known issue]
A routed message drops its attachments, and a refused message's text is lost
rather than kept in history ([#414](https://github.com/joestump-agent/crush/issues/414)). The grammar is strict ([#415](https://github.com/joestump-agent/crush/issues/415)):
`@tester: stop` and `@tester, stop` go to the main agent, and a lone
`@Makefile` is swallowed by the empty-message warning although no agent has
that handle.
:::

## Mid-sentence mentions: agent cards

A handle anywhere after the first token is a **mention**. Mentions never route:
the main agent gets your message unchanged, plus one agent card per distinct
agent mentioned.

```text
Is @tester covering the empty-input case, or should @docs-writer note it?
```

A card is a Markdown attachment, `agent-<handle>.md`. A live agent's card
carries its handle, role, status, current todo, and session ID, and says how to
steer it. A finished agent's card is marked read-only and adds its key findings
and diff summary.

A mention must start a word and end at whitespace or a line end, so `@tester?`
and `@tester's` attach nothing ([#415](https://github.com/joestump-agent/crush/issues/415)). Unknown handles stay plain text.

## @ completions

Typing <kbd>@</kbd> lists live dispatched agents first, above files and MCP
resources, from every session in this Crush process:

```text
@tester        tester · ● · working · Writing edge-case tests
@docs-writer   docs writer · ○ · queued
```

Each row shows the role, a status dot (● working, ○ queued), and the current
todo. Finished agents never appear. Selecting a row inserts `@<handle>`.

:::warning[Known issue]
In a git repository, typing <kbd>@</kbd> creates `.crush/worktrees` in the
launch directory even if nothing was dispatched. Launched from a subdirectory,
that new `.crush` can change the data directory the next launch picks ([#370](https://github.com/joestump-agent/crush/issues/370)).
:::

## Handles and @file mentions

Handles never contain `.` or `/`, so `@main.go` and `@internal/ui` are always
files. A bare token such as `@Makefile` is a handle candidate, and if an agent
has that handle, the agent wins. A file attached from the completions is then
dropped from the routed message ([#414](https://github.com/joestump-agent/crush/issues/414)).

## The `message_agent` tool

`message_agent` is the main agent's version of a leading @handle: ask it to
"tell @tester to skip the benchmarks" and it calls the tool with a `handle` (or
the agent's `session_id`) and a `message`. The message is queued the same way,
the tool returns once it's queued, and the reply appears on the agent's block.
Unknown and finished agents are refused. Only the main agent has the tool.

:::warning[Known issue]
The schema marks `session_id` required, though either field works ([#400](https://github.com/joestump-agent/crush/issues/400)).
:::

## The agent block

Each `dispatch_agent` call renders as an agent block in the main chat. Its
header reads like `tester · working · 2m14s · 14.2K tokens · 2/5 todos`: handle,
state (`queued`, `working`, `complete`, `failed`, `killed` — shown as `canceled` when you stopped it yourself), elapsed time,
tokens, and todo progress. Below it come the task prompt, the current todo
(`→ …`), the messages you've sent with the agent's replies, and its tool calls.

When the agent finishes, the block becomes a durable record that never clears:
`Killed` (rendered as `Canceled` with the reason `canceled by user` when you
stopped it with <kbd>ctrl+x</kbd>) or `Error` with a reason if the run didn't complete, then
**Findings** (its final message) and **Diff** (per-file stat and diff, cut off
at 250 lines). Focus the block and press <kbd>space</kbd> to expand clipped
sections.

`agent`-tool blocks can be inspected too, but have no handle and can't be
steered or canceled — <kbd>ctrl+x</kbd> is dispatch-only.

:::warning[Known issue]
Messages you send an agent are kept only in memory ([#410](https://github.com/joestump-agent/crush/issues/410)). They vanish from
the block whenever the transcript is rebuilt, leaving inspect mode included,
and messages sent while inspecting never show. After a restart, a finished
agent's block reads `working`, from the handle saved at dispatch.
:::

## Inspect mode

Inspect mode shows a sub-agent's full transcript — reasoning, tool calls,
results, todo nudges — in the chat window while the main session stays active.

| Key | Not inspecting | Inspecting |
| --- | --- | --- |
| <kbd>ctrl+]</kbd> | Open the selected agent block, or the first live agent if none is selected | Cycle to the next live agent |
| <kbd>ctrl+[</kbd> | Acts as <kbd>esc</kbd> where the terminal can't tell them apart | Back to the chat, scroll restored |
| <kbd>ctrl+x</kbd> | Cancel the selected live dispatch block | Cancel the dispatch you are viewing |
| <kbd>esc</kbd> | Unchanged | Depends on the terminal; see below |

- The keys work from the editor or the chat; an open dialog takes them first.
  With no live agents, Crush reports `No live sub-agents to inspect`.
- The cycle ring is a snapshot of live agent blocks taken on entry; agents
  dispatched later join on the next entry.
- A live transcript follows the stream.
- The editor placeholder reads
  `Inspecting <title> (2/3) · ctrl+[ returns · prompts go to the parent`, with
  the counter only when several agents are live. Yolo mode replaces it with
  `Go crazy`.
- **Viewed is not active.** Prompts, `/compact`, the sidebar, todo pills, and
  history stay with the main session. Its new messages don't render while you
  inspect; the transcript reloads when you return.
- Switching sessions or <kbd>ctrl+n</kbd> leaves inspect mode.

:::warning[Known issue]
<kbd>esc</kbd> depends on the terminal ([#404](https://github.com/joestump-agent/crush/issues/404)). Without the kitty keyboard
protocol (macOS Terminal.app, tmux), <kbd>ctrl+[</kbd> arrives as
<kbd>esc</kbd>, so <kbd>esc</kbd> leaves inspect mode. With it (kitty, Ghostty,
WezTerm, foot), <kbd>esc</kbd> keeps its chat meaning: while the main agent is
busy, it clears any queued prompts (a pending dispatch result included), and a
double press cancels the main agent's turn, stopping any `agent` sub-agent
you're watching. Use <kbd>ctrl+[</kbd> to leave.
:::

:::info[Planned]
<kbd>esc</kbd> and <kbd>ctrl+[</kbd> will both leave inspect mode on every
terminal, and <kbd>esc</kbd> will never cancel the main agent from inspect mode
([#404](https://github.com/joestump-agent/crush/issues/404)).
:::

:::warning[Known issue]
- Finished `agent`-tool blocks count as live, so <kbd>ctrl+]</kbd> can open the
  oldest finished agent, and the ring and counter include finished agents
  ([#405](https://github.com/joestump-agent/crush/issues/405)).
- Entering from a finished block makes the first <kbd>ctrl+]</kbd> skip the
  first live agent, and the ring misses agents with no block in the transcript
  ([#416](https://github.com/joestump-agent/crush/issues/416)).
- A2UI forms in an inspected transcript stay live; submitting one starts a turn
  on the main agent ([#407](https://github.com/joestump-agent/crush/issues/407)).
- The inspect keys are missing from <kbd>ctrl+g</kbd> help ([#412](https://github.com/joestump-agent/crush/issues/412)).
:::

## Sessions tree

In the session picker (<kbd>ctrl+s</kbd>), a session with sub-agent sessions
shows `▸N` before its timestamp.

| Key | Session list | Sub-agent list |
| --- | --- | --- |
| <kbd>enter</kbd> | Open the session | Open the sub-agent in inspect mode |
| <kbd>ctrl+]</kbd> | List its sub-agents (`Sessions ▸ <title>`) | — |
| <kbd>esc</kbd> | Close the picker | Back to the session list |
| <kbd>ctrl+r</kbd> / <kbd>ctrl+x</kbd> | Rename / delete | Disabled |

The list holds every agent-tool task session: dispatched agents, `agent`
sub-agents, and `agentic_fetch` runs, but not title-generation helpers. If the
sub-agent's parent isn't the active session, Crush loads the parent first, so
prompts go to the session the transcript belongs to. Sub-agent sessions open
read-only and can't be continued; dispatch a new agent instead.

:::warning[Known issue]
- `crush -s <sub-agent session id>` and `crush run --continue` can still make a
  sub-agent session active ([#413](https://github.com/joestump-agent/crush/issues/413)).
- Sub-agents are listed in an order that shifts as they work ([#417](https://github.com/joestump-agent/crush/issues/417)).
- Deleting a session leaves its sub-agent sessions behind, unlisted ([#418](https://github.com/joestump-agent/crush/issues/418)).
:::

## Client/server mode

In [client/server mode](/features/server-and-workspaces)
(`CRUSH_CLIENT_SERVER=1`), the dispatch registry and message queue live in the
server, and no API exposes them yet.

| Surface | Behavior |
| --- | --- |
| Agent block | Static: the handle saved at dispatch, never updated |
| @ completions | No agents |
| Leading @handle | Sent to the main agent as an ordinary prompt, without warning |
| Mentions | No card |
| Inspect mode | A selected block opens, but there are no live agents to cycle |
| Sessions tree | Works |

:::info[Planned]
The TUI becomes an A2A client of the per-process A2A host, so status, steering,
and cancellation work the same in both modes ([#421](https://github.com/joestump-agent/crush/issues/421)).
:::
