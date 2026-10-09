---
id: overview
title: Dispatched agents
sidebar_label: Overview
sidebar_position: 1
description: Background agents that work in their own git worktrees while the main agent keeps going — what they are, when to use them, and what happens to their work.
---

# Dispatched agents

:::info[Fork feature]
Multi-agent dispatch — the `dispatch_agent`, `message_agent`,
`cancel_dispatch`, `apply_dispatch` and `dismiss_dispatch` tools, @handles,
inspect mode, the sessions tree and agent definitions — is an addition in the
`joestump-agent/crush` fork.
:::

The main agent can hand an independent subtask to a **dispatched agent** with
the `dispatch_agent` tool. The dispatched agent works in its own git worktree on
a fresh branch, in the background, and reports back with its findings and a diff
when it finishes. The tool returns a running handle at once, so the main agent
keeps working, or dispatches more agents, in the meantime.

Crush never merges on its own. You, or the main agent at your direction, review
the diff and keep it with `apply_dispatch` or drop it with `dismiss_dispatch`.

## `agent` or `dispatch_agent`?

| | `agent` | `dispatch_agent` |
| --- | --- | --- |
| Runs | In the foreground; the main agent waits | In the background; the tool returns immediately |
| Which agent | Always the `task` definition | The call's `agent` argument, else `options.dispatch.default_agent` — the `worker` definition unless you change it |
| Tools | Read-only research tools | The dispatch agent's definition. The built-in `worker` gets the read tools plus `bash`, `edit`, `multiedit`, `write`, `todos`, `job_output`, `job_kill` and `lsp_diagnostics`, and `question` while your session is interactive |
| Works in | Your checkout, uncommitted changes included | Its own worktree on a `crush-dispatch-<id>` branch, cut from a commit |
| Model | The large model | The dispatch agent's model slot — `small` for the built-in `worker` — unless the call picks `large` or `small` |
| Reachable mid-run | No | Yes, by its @handle |
| Result | The tool result, in the same turn | A follow-up turn on the main agent when it finishes |

## When to dispatch

Dispatch work that doesn't depend on what the main agent is doing and touches
different files: tests for a module you aren't changing, a docs update, a probe,
a refactor of an unrelated package.

Don't dispatch work that needs your uncommitted changes, because the agent never
sees your working tree. Don't dispatch read-only searches either; the `agent`
tool handles those. A dispatched agent reads your context files (`AGENTS.md`,
`CRUSH.md`, `CLAUDE.md`, and the global ones) from its worktree, and it can
ask you a question: the question appears in your question prompt labeled with
its `@handle`, and the agent waits for your answer. Still, write the request
so it stands on its own — a self-contained prompt finishes faster than one that
has to ask.

## Requirements

- **A git repository.** Outside one, the tool returns
  `dispatch unavailable: … is not a git repository`, and Crush caches that
  failure until it restarts.
- **Committed work.** The worktree is cut from your current branch (or `HEAD`
  when detached), or from the revision the main agent passes as `branch`.
- **The interactive TUI.** The five dispatch tools are offered to the main
  agent only while Crush is interactive. `crush run` never offers them, by
  design: the process exits when the turn ends, so a dispatch could not report
  back.
- **An enabled dispatch agent.** `agent set worker --disabled true` (with no
  other dispatch agent defined) removes all five tools.

## The four surfaces

| Surface | What it gives you |
| --- | --- |
| [The agent block](/agents/handles-and-inspect#the-agent-block) | A live status card in the main chat that becomes a durable record of findings and diff. |
| [@handles](/agents/handles-and-inspect#handles) | `@tester …` at the start of a message steers a running agent. A mid-sentence `@tester` gives the main agent its status card. |
| [Inspect mode](/agents/handles-and-inspect#inspect-mode) | <kbd>ctrl+]</kbd> shows an agent's full transcript in the chat window; <kbd>esc</kbd> or <kbd>ctrl+[</kbd> returns. |
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
| `killed` | killed, or canceled when you stopped it | Crush stopped it — `ignored nudges`, `stalled todos`, `hard timeout`, `tool loop`, `crush exited`, or `idle timeout` for an external agent — or you did, with <kbd>ctrl+x</kbd> or `cancel_dispatch` (`canceled by user`). |

Dispatched agents must keep a todo list. By default, one that ignores two
nudges about it is killed. See [Todo enforcement](/agents/todo-enforcement).

At most `options.dispatch.max_concurrent` agents run at once — **4** unless
you change it (`option dispatch-max-concurrent`). A dispatch past the cap
fails with `dispatch at capacity: N agents are already running
(dispatch.max_concurrent=M); wait for one to finish or cancel one`, and
provisions nothing.

Stop one dispatched agent yourself: focus its block in the chat (or inspect
it) and press <kbd>ctrl+x</kbd> — the run ends killed with reason
`canceled by user`, the card shows **canceled**, and the workspace and branch
stay for review or a re-dispatch. You can also ask the main agent, which
cancels by handle through its `cancel_dispatch` tool. The main agent's own
cancel is unchanged: <kbd>esc</kbd> twice cancels only the parent's turn.

## What happens to the work

When a dispatched agent finishes, the main agent gets a hidden follow-up turn
telling it to review the work, with this JSON:

| Field | Contents |
| --- | --- |
| `dispatch_id`, `handle`, `session_id`, `agent` | Which agent this was, and the definition it ran (`worker` unless the call named another) |
| `branch`, `workspace_path` | `crush-dispatch-<id>` and `<data dir>/worktrees/<repo-key>/crush-dispatch-<id>` |
| `status` | `completed`, `failed`, or `killed` |
| `key_findings` | The agent's final message; for a killed run, its last state |
| `diff_summary` | A per-file `+/-` stat, then the diff, cut off at 250 lines. Committed and uncommitted changes, new files included |
| `error`, `killed_reason` | Why it failed or was killed |
| `undelivered_steers` | Messages you sent that the agent never read because its run ended first |
| `source` | External agents only: the card URL the work came from. The findings are labeled untrusted |

The main agent may act on the result right away, applying it included, subject
to your permission prompts. Tell it up front if you want to review first. The
dispatched session's cost is added to the parent session's.

You record your decision with two tools, each of which asks your permission
before it changes anything:

- **`apply_dispatch`** brings the work into your checkout — `merge` (the
  default, a `--no-ff` merge commit), `squash` (the work lands **staged and
  uncommitted**, for you to shape the commit) or `cherry-pick` — and removes
  the workspace. Uncommitted changes in the worktree are committed on the
  dispatch branch first, so every mode carries them. It refuses, changing
  nothing, while the dispatch is still running, when your own checkout is
  dirty or mid-merge, and on a conflict (the conflicting paths are listed and
  the merge aborted).
- **`dismiss_dispatch`** discards the work, removes the worktree and branch,
  and frees the handle. There is no undo.

:::info
**Quitting keeps dispatch work.** At exit, Crush releases only dispatches
you applied or dismissed and ones that produced no work; anything with
commits or uncommitted changes stays on disk and on its branch, and only
this process's own workspaces are touched. A crash removes nothing; the next
launch reconciles the same way.
:::

The worktrees live under your data directory, not your checkout: `crush dirs`
prints it (`.crush` in the project by default), and
`<data dir>/worktrees/.gitignore` is `*`, so nothing shows up in `git status`
whichever directory you launched from. From the shell,
`crush dispatch list` summarizes every workspace (id, handle, branch,
disposition, owner liveness, pending changes; `--json` for scripts) and
`crush dispatch prune` removes decided ones, or every dead owner's with
`--all-dead` (`--force` to include ones holding changes); `--dry-run`
previews without deleting.

## Under the hood: A2A

Every Crush process runs one [A2A](https://a2a-protocol.org) host on a `0600`
unix socket under the data directory. It builds one Agent Card per agent
definition (`coder`, `plan`, `task`, `worker` and any agent you add), each
with a route at `/agents/<id>`, and one route per running dispatch, named
after its @handle. Dispatched agents and the `agentic_fetch` sub-agent run on
that runtime: Crush drives each run through an A2A client, reading its event
stream to a terminal state, and a kill is a `tasks/cancel` carrying its
reason. Task state lives in the SQLite session database, so a crash is
reconciled at the next start. Delegation is one level deep: neither `agent`
sub-agents nor dispatched agents get the dispatch tools.

The host can also listen on TCP, TLS only, as an opt-in. A remote peer with
the right client certificate is authenticated and reaches your agents'
routes, but it cannot run anything on your host — every task is rejected —
until the peer registry
([#334](https://github.com/joestump-agent/crush/issues/334)) lands, and the
cards themselves are not yet served over the wire.
What a team can do today is on [Working with other agents](/agents/team);
the wire is on [A2A protocol](/agents/a2a-protocol) and the components on
[Architecture](/agents/architecture).

## Turning it off

Only the main coder agent gets the five dispatch tools; plan mode, sub-agents
and dispatched agents never do. Either deny them, or disable the `worker`
definition — with no enabled dispatch agent the tools disappear together:

```bash
# crushrc — either line is enough
permissions deny dispatch_agent message_agent cancel_dispatch apply_dispatch dismiss_dispatch
agent set worker --disabled true
```

See [Denying tools](/configuration/permissions#denying-tools), and the
[configuration reference](/agents/configuration) for everything else.
