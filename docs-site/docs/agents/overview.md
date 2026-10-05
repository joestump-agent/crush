---
id: overview
title: Dispatched agents
sidebar_label: Overview
sidebar_position: 1
description: Background agents that work in their own git worktrees while the main agent keeps going — what they are, when to use them, and what happens to their work.
---

# Dispatched agents

:::info[Fork feature]
Multi-agent dispatch — the `dispatch_agent` and `message_agent` tools, @handles,
inspect mode, and the sessions tree — is an addition in the
`joestump-agent/crush` fork.
:::

The main agent can hand an independent subtask to a **dispatched agent** with
the `dispatch_agent` tool. The dispatched agent works in its own git worktree on
a fresh branch, in the background, and reports back with its findings and a diff
when it finishes. The tool returns a running handle at once, so the main agent
keeps working, or dispatches more agents, in the meantime.

Crush never merges the result. You, or the main agent at your direction, review
the diff and decide what to keep.

## `agent` or `dispatch_agent`?

| | `agent` | `dispatch_agent` |
| --- | --- | --- |
| Runs | In the foreground; the main agent waits | In the background; the tool returns immediately |
| Tools | Read-only research tools | Read tools plus `bash`, `edit`, `multiedit`, `write`, and `todos` |
| Works in | Your checkout, uncommitted changes included | Its own worktree on a `crush-dispatch-<id>` branch, cut from a commit |
| Model | The large model | The **small** model, unless the main agent asks for `large` |
| Reachable mid-run | No | Yes, by its @handle |
| Result | The tool result, in the same turn | A follow-up turn on the main agent when it finishes |

## When to dispatch

Dispatch work that doesn't depend on what the main agent is doing and touches
different files: tests for a module you aren't changing, a docs update, a probe,
a refactor of an unrelated package.

Don't dispatch work that needs your uncommitted changes, because the agent never
sees your working tree. Don't dispatch read-only searches either; the `agent`
tool handles those. A dispatched agent can't ask you questions, so the request
must stand on its own.

:::warning[Known issue]
Dispatched agents don't get your context files (`AGENTS.md`, `CRUSH.md`,
`CLAUDE.md`) ([#386](https://github.com/joestump-agent/crush/issues/386)). Put any convention that matters in your request so the
main agent passes it on.
:::

## Requirements

- **A git repository.** Outside one, the tool returns
  `dispatch unavailable: … is not a git repository`, and Crush caches that
  failure until it restarts.
- **Committed work.** The worktree is cut from your current branch (or `HEAD`
  when detached), or from the revision the main agent passes as `branch`.
- **The interactive TUI, running in-process.**

:::warning[Known issue]
`crush run` offers `dispatch_agent`, but exits when the main agent's turn ends.
Any running dispatch is torn down and its result never arrives ([#387](https://github.com/joestump-agent/crush/issues/387)). In
[client/server mode](/features/server-and-workspaces) (`CRUSH_CLIENT_SERVER=1`)
the agent block is static, the @ completions list no agents, and a leading
`@handle` goes to the main agent as an ordinary prompt ([#421](https://github.com/joestump-agent/crush/issues/421), [#419](https://github.com/joestump-agent/crush/issues/419)).
:::

## The four surfaces

| Surface | What it gives you |
| --- | --- |
| [The agent block](/agents/handles-and-inspect#the-agent-block) | A live status card in the main chat that becomes a durable record of findings and diff. |
| [@handles](/agents/handles-and-inspect#handles) | `@tester …` at the start of a message steers a running agent. A mid-sentence `@tester` gives the main agent its status card. |
| [Inspect mode](/agents/handles-and-inspect#inspect-mode) | <kbd>ctrl+]</kbd> shows an agent's full transcript in the chat window; <kbd>ctrl+[</kbd> returns. |
| [The sessions tree](/agents/handles-and-inspect#sessions-tree) | In <kbd>ctrl+s</kbd>, <kbd>ctrl+]</kbd> on a session lists its sub-agent sessions. |

[Your first parallel task](/agents/tutorial) walks through all four.

## Lifecycle

```text
dispatch_agent ──▶ provisioned ──▶ running ──┬──▶ completed
 (worktree +        "queued"      "working"  ├──▶ failed
  branch)                                    └──▶ killed
```

| State | Card label | Meaning |
| --- | --- | --- |
| `provisioned` | queued | The worktree exists; the agent hasn't started yet. Brief. |
| `running` | working | The agent is working, and you can steer it. |
| `completed` | complete | It finished, and the main agent has the result. |
| `failed` | failed | The run errored; the card shows why. |
| `killed` | killed, or canceled when you stopped it | Crush stopped it — ignored todo nudges, stalled todos, a hard timeout, or a tool loop — or you did with <kbd>ctrl+x</kbd>. |

Dispatched agents must keep a todo list. By default, one that ignores two
nudges about it is killed. See [Todo enforcement](/agents/todo-enforcement).

:::warning[Known issue]
The `killed` state doesn't appear today. A kill for nudges, stalled todos, or a
hard timeout hangs the dispatch until the 3-minute limit reports it as failed;
a tool loop reports as completed ([#342](https://github.com/joestump-agent/crush/issues/342), [#343](https://github.com/joestump-agent/crush/issues/343)).
:::

Stop one dispatched agent yourself: focus its block in the chat (or inspect
it) and press <kbd>ctrl+x</kbd> — the run ends killed with reason "canceled by
user", the card shows **canceled**, and the workspace and branch stay for
review or a re-dispatch. You can also ask the main agent, which cancels by
handle through its `cancel_dispatch` tool. The main agent's own cancel is
unchanged: <kbd>esc</kbd> twice cancels only the parent's turn.

## What happens to the work

When a dispatched agent finishes, the main agent gets a hidden follow-up turn
telling it to review the work, with this JSON:

| Field | Contents |
| --- | --- |
| `dispatch_id`, `handle`, `session_id` | Which agent this was |
| `branch`, `workspace_path` | `crush-dispatch-<id>` and `<cwd>/.crush/worktrees/crush-dispatch-<id>` |
| `status` | `completed`, `failed`, or `killed` |
| `key_findings` | The agent's final message; for a killed run, its last state |
| `diff_summary` | A per-file `+/-` stat, then the diff, cut off at 250 lines. Committed and uncommitted changes, new files included. |
| `error`, `killed_reason` | Why it failed or was killed |

The main agent may act on the result right away, merging included, subject to
your permission prompts. Tell it up front if you want to review first. The
dispatched session's cost is added to the parent session's.

:::warning[Known issue]
**Quitting Crush deletes dispatch work.** On exit, Crush force-removes every
`crush-dispatch-*` worktree under `.crush/worktrees` and deletes its branch
with `git branch -D`. That includes unmerged results, killed runs, and other
Crush instances' worktrees in the same directory. Depending on how you quit,
removal may be partial ([#367](https://github.com/joestump-agent/crush/issues/367), [#365](https://github.com/joestump-agent/crush/issues/365)). Merge what you want before quitting,
and don't run two dispatching instances in one directory.
:::

:::info[Planned]
Workspaces will be preserved for salvage, never discarded at exit ([#367](https://github.com/joestump-agent/crush/issues/367)).
You'll apply or dismiss them explicitly with `apply_dispatch` and
`dismiss_dispatch` ([#368](https://github.com/joestump-agent/crush/issues/368)), and manage them from the shell with
`crush dispatch list` and `crush dispatch prune` ([#369](https://github.com/joestump-agent/crush/issues/369)).
:::

## Under the hood: A2A

Each dispatch is served in-process by an [A2A](https://a2a-protocol.org)
server whose agent card is named after the @handle and described by the role.
Crush drives the run through an A2A client, reading its event stream to a
terminal state. Today that server is an unauthenticated JSON-RPC endpoint on a
random `127.0.0.1` port per dispatch: an internal detail, not an interface for
outside clients. Delegation is one level deep: neither `agent` sub-agents nor
dispatched agents get `dispatch_agent` or `message_agent`. See
[Architecture](/agents/architecture) and [A2A protocol](/agents/a2a-protocol).

:::info[Planned]
A2A becomes the runtime contract: one execution path through the A2A client
([#347](https://github.com/joestump-agent/crush/issues/347)), one A2A host per Crush process on a `0600` unix socket ([#346](https://github.com/joestump-agent/crush/issues/346)),
TCP plus TLS as an opt-in ([#358](https://github.com/joestump-agent/crush/issues/358)), and later the other agents too ([#392](https://github.com/joestump-agent/crush/issues/392)).
:::

:::info[Partially shipped]
Agent definitions: the built-in `coder`, `plan`, `task`, and a new `worker`
agent for dispatch become overridable defaults you can extend. The
`crush.json` data model and its validation are live
([#333](https://github.com/joestump-agent/crush/issues/333)); still planned:
the `crushrc` builtin ([#431](https://github.com/joestump-agent/crush/issues/431)) and an `agent`
parameter on `dispatch_agent` to choose one ([#433](https://github.com/joestump-agent/crush/issues/433)).
:::

## Turning it off

Only the main coder agent gets these tools; plan mode never does. Remove them
with `permissions deny` in `crushrc`, or `options.disabled_tools` in
`crush.json`:

```bash
permissions deny dispatch_agent message_agent
```

See [Denying tools](/configuration/permissions#denying-tools), and the
[configuration reference](/agents/configuration) for everything else.
