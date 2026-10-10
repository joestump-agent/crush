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

Handles are unique among **running** agents across the Crush process. A
handle that is already held by a live agent, or that is one of the reserved
names `coder`, `plan`, `task`, `worker` and `all`, gets a numeric suffix:
`tester-2`, then `tester-3`. Assignment is atomic, so agents dispatched
together never collide. A handle is at most 32 bytes; a longer slug is cut,
and the cut makes room for the suffix.

A handle lives as long as its run. When the agent finishes, its handle is free
for the next dispatch — a second `tester` after the first completed is plain
`tester` again. The finished agent's block and card keep the handle they were
given.

The handle shows in the agent block's header, the @ completions, the dispatch
result's `handle` field, and as the name on the agent's A2A card.

## Leading @handle: steering

A message whose first token is a handle goes to that agent, not the main agent:

```text
@tester skip the benchmarks; just the table tests
```

The handle must open the first line (leading spaces are ignored) and end at
whitespace, the end of the message, or one of `: , . ? !` when that character
is itself followed by whitespace or the end — `@tester: stop` and
`@tester, stop` both route, with the punctuation dropped. Handle tokens are
letters, digits, `-`, and `_`, matched case-insensitively.

| You send | What happens |
| --- | --- |
| A running agent's handle, then text | Queued for that agent. Your message and its reply show on its block; nothing lands in the main conversation. |
| A finished agent's handle, then text | Refused: `agent @tester finished (completed); task sessions are never continuable — dispatch a new agent instead`. The text stays in the editor. |
| An external agent's handle, then text | Refused: `steering external agents is not supported yet`. Cancel and re-dispatch instead. |
| A handle-shaped token alone | Warning: `Nothing to send @handle — write the message after the handle, e.g. "@tester stop writing Rust".` |
| Anything else, including an unknown handle | An ordinary prompt to the main agent. |

Handles resolve against the agents dispatched from your current session's
tree. A queued message is folded into the agent's turn at its next model step,
or runs as its immediate follow-up turn, in the order you sent it. If the run
ends before the agent reads it, the dispatch result lists it under
`undelivered_steers`.

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

A mention must start a word and end the same way a leading handle does, so
`@tester?` and `@tester,` attach a card while `@tester's` is prose. Unknown
handles stay plain text.

## @ completions

Typing <kbd>@</kbd> lists live dispatched agents first, above files and MCP
resources:

```text
@tester        tester · ● · working · Writing edge-case tests
@docs-writer   docs writer · ○ · queued
```

Each row shows the role, a status dot (● working, ○ queued), and the current
todo. Finished agents never appear. Selecting a row inserts `@<handle>`.

## Handles and @file mentions

Handles never contain `.` or `/`, so `@main.go` and `@internal/ui` are always
files. A bare token such as `@Makefile` is a handle candidate only if an agent
has that handle; otherwise it is an ordinary file mention or plain text.

## The `message_agent` tool

`message_agent` is the main agent's version of a leading @handle: ask it to
"tell @tester to skip the benchmarks" and it calls the tool with exactly one
of `handle` or `session_id`, plus a `message`. The message is queued the same
way, the tool returns once it's queued, and the reply appears on the agent's
block. Unknown, finished and external agents are refused. Only the main agent
has the tool, and only for agents dispatched from its own session.

`message_agent`, `cancel_dispatch`, `apply_dispatch` and `dismiss_dispatch` act
only on agents dispatched from the calling session: another session's handle,
dispatch ID or session ID is refused exactly like an unknown one
([#399](https://github.com/joestump-agent/crush/issues/399),
[#559](https://github.com/joestump-agent/crush/issues/559)).

## The agent block

Each `dispatch_agent` call renders as an agent block in the main chat. Its
header reads like `tester · working · 2m14s · 14.2K tokens · 2/5 todos`: handle,
state (`queued`, `working`, `complete`, `failed`, `killed` — shown as
`canceled` when you stopped it yourself), elapsed time, tokens, and todo
progress. Below it come the task prompt, the current todo (`→ …`), the
messages you've sent with the agent's replies, and its tool calls.

When the agent finishes, the block becomes a durable record that never clears:
`Killed` (rendered as `Canceled` with the reason `canceled by user` when you
stopped it with <kbd>ctrl+x</kbd>) or `Error` with a reason if the run didn't
complete, then **Findings** (its final message) and **Diff** (per-file stat
and diff, cut off at 250 lines). Focus the block and press <kbd>space</kbd> to
expand clipped sections. The terminal state is stamped on the parent's
`dispatch_agent` tool result when the result is delivered, so a reloaded or
restarted session renders the finished block from that record.

`agent`-tool blocks can be inspected too, but have no handle and can't be
steered or canceled — <kbd>ctrl+x</kbd> is dispatch-only.

## Inspect mode

Inspect mode shows a sub-agent's full transcript — reasoning, tool calls,
results, todo nudges — in the chat window while the main session stays active.

| Key | Not inspecting | Inspecting |
| --- | --- | --- |
| <kbd>ctrl+]</kbd> | Open the selected agent block, or the first live agent if none is selected | Cycle to the next live agent |
| <kbd>esc</kbd> or <kbd>ctrl+[</kbd> | Unchanged (<kbd>esc</kbd> keeps its chat meaning) | Back to the chat, scroll restored. Works on every terminal; <kbd>esc</kbd> never cancels the main agent from here |
| <kbd>ctrl+x</kbd> | Cancel the selected live dispatch block | Cancel the dispatch you are viewing |

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

## Sessions tree

In the session picker (<kbd>ctrl+s</kbd>), a session with sub-agent sessions
shows `▸N` before its timestamp. The tree loads right after the picker
opens: until it lands, <kbd>ctrl+]</kbd> reports "Loading sub-agent
sessions…", and a failed load warns once while leaving the picker usable.

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
read-only and can't be continued; dispatch a new agent instead. Neither
`crush -s <id>` nor `crush run --continue` will make a task session the active
one.

## Client/server mode

In [client/server mode](/features/server-and-workspaces#dispatched-agents-against-a-server)
(`CRUSH_CLIENT_SERVER=1`), every surface on this page works as it does in
process. The TUI follows the server's dispatched agents over A2A, through the
server's proxy to the workspace's agent host:
- the agent block updates live;
- the @ completions list running agents;
- a leading `@handle` steers the agent;
- mentions attach a card;
- <kbd>ctrl+x</kbd> cancels an agent.

A finished agent's card carries its findings once the run has stamped its
result on the dispatch card. Until then it carries the agent's final status
text.
