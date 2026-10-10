---
id: tutorial
title: Your first parallel task
sidebar_label: Tutorial
sidebar_position: 2
description: Dispatch a background agent, watch it, steer it, read its transcript, and bring its work in — end to end.
---

# Your first parallel task

:::info[Fork feature]
Multi-agent dispatch is an addition in the `joestump-agent/crush` fork. Start
with the [overview](/agents/overview) if you haven't read it.
:::

The main agent fixes a bug while a dispatched agent writes tests in parallel.
You'll steer the agent, ask about it, read its transcript, and apply its work.

## Before you start

1. **Run Crush inside a git repository with at least one commit.** It can be
   any directory of the repository: dispatch worktrees go under Crush's data
   directory (`crush dirs` shows it), not under your checkout, and that
   directory ignores itself, so `git status` stays clean.
2. **Commit what the agent should see.** A dispatched agent starts from your
   current branch's last commit, not from your working tree.
3. **Decide how its permission prompts get answered.** The dispatched agent's
   `bash`, `edit` and `write` requests appear in your usual permission dialog,
   prefixed with its `@handle`, whenever the TUI is up — the main agent does not
   have to be mid-turn. Three ways to avoid answering each one:
   - toggle yolo with <kbd>ctrl+y</kbd>, which applies to every running
     dispatched agent's next request too, or start with `crush --yolo`;
   - pre-allow the tools: `permissions allow bash edit multiedit write`. The
     list is read from your session's config and applies to the main agent as
     well. See [Allowing tools](/configuration/permissions#allowing-tools);
   - answer the prompts as they come. A dispatched agent's run pauses on its
     request while the other agents keep going.

## Ask for parallel work

Ask for the split in plain language. Name a role, and pass along anything the
agent should know that is not in your checkout's context files — the
dispatched agent reads `AGENTS.md` and friends from your checkout, not from
the branch it works on
([#561](https://github.com/joestump-agent/crush/issues/561)).

```text
Fix the off-by-one in pkg/x/parse.go yourself. In parallel, dispatch a
"tester" to add table-driven tests for pkg/x/lexer.go. It should use
testify's require package, and run only `go test ./pkg/x/...`.
```

The main agent calls `dispatch_agent` with a self-contained `prompt` and,
usually, a `role` and a `handle`. If it passes no handle, the role is slugged
into one (`docs writer` becomes `docs-writer`). With neither, the handle is
`agent`. A handle that is already held by a **running** agent, or that is a
reserved name (`coder`, `plan`, `task`, `worker`, `all`), gets a numeric
suffix: `tester-2`, `tester-3`. Finished agents release their handles.

The dispatch runs on the `worker` agent definition with the small model
unless you say otherwise. "On the large model" switches the slot for one
dispatch; "with the `tester` agent" picks another
[agent definition](/agents/configuration#agent-definitions) you have added.

The tool returns at once with `{"dispatch_id", "handle", "agent", "branch",
"workspace_path", "session_id", "status": "running"}`, and the main agent
carries on with the parser fix.

## Watch the block

An agent block appears in the main chat. While the agent runs, it looks
roughly like this:

```text
● Dispatch tester · working · 1m12s · 14.2K tokens · 2/5 todos

  Task  Add table-driven tests for pkg/x/lexer.go …
        → Writing edge-case tests for unterminated strings
  ├─ View pkg/x/lexer.go
  ╰─ Edit pkg/x/lexer_test.go
```

The header shows the handle, state, elapsed time, tokens, and todos done. The
`→` line is the todo in progress, and the agent's tool calls nest below it.

When the agent finishes, the block stays put as a durable record. It shows
**Findings** (the agent's final message) and **Diff** (a per-file stat and the
diff), plus **Error** or **Killed** with a reason if the run didn't complete.
Focus the block and press <kbd>space</kbd> to expand it.

At the same moment, the main agent gets the result as a hidden follow-up turn
and replies with its review.

If the main agent is busy when the result arrives, the delivery waits
outside the prompt queue and is delivered when the agent next goes
idle — pressing <kbd>esc</kbd> to clear the queue or canceling does not
discard it. Results that stack up arrive in one turn.

## Steer an agent

Start a message with the agent's handle to send it a correction mid-run:

```text
@tester use require.Equal, not assert.Equal
```

The handle must be the very first token. It ends at a space, the end of the
line, or one of `: , . ? !` followed by a space, so `@tester: skip the
benchmarks` routes too; `@tester's` is prose. The message goes to that agent's
queue, and the agent picks it up at its next step. Your message appears on the
agent's block on a `→` line, with the agent's reply beneath it. Nothing lands
in the main conversation.

If the agent has already finished, Crush refuses with
`agent @tester finished (completed); task sessions are never continuable — dispatch a new agent instead`.
The text you typed stays in the editor: clear it before your next prompt, or
the next message starts with `@tester` and is refused again. To follow up,
ask the main agent to dispatch a new agent.

## Ask about an agent

Mention the handle anywhere *after* the first word to ask the main agent about
it:

```text
How far along is @tester, and is it covering the empty-input case?
```

Crush attaches the agent's card to your message: handle, role, status, current
todo, and session. For a finished agent, the card adds its findings and diff
summary. The main agent answers from the card. A mention never routes a message
to the agent itself. The same punctuation rule applies: `@tester,` and
`@tester?` mention; `@tester's` does not.

## Inspect its transcript

To read everything the agent did, including its reasoning, every tool call and
result, and any todo nudges it got:

1. Press <kbd>tab</kbd> to focus the chat, then select the agent block with
   <kbd>shift+↑</kbd> / <kbd>shift+↓</kbd> (or click it).
2. Press <kbd>ctrl+]</kbd>.

The chat window now shows the agent's live transcript, which follows the
stream as the agent works. If no agent block is selected, <kbd>ctrl+]</kbd>
opens the first live agent; with none live it reports
`No live sub-agents to inspect`. Press <kbd>ctrl+]</kbd> again to cycle
through the live agents. Press <kbd>esc</kbd> or <kbd>ctrl+[</kbd> to go
back; the chat's scroll position is restored. Both keys work on every
terminal, and <kbd>esc</kbd> never cancels the main agent from inspect mode.

While you inspect, the editor is read-only: typing, paste and submit are
ignored, so nothing can land unseen in the main conversation. The editor
placeholder reminds you which agent is on screen:
`Inspecting @handle · esc returns · editor read-only`. In yolo mode the
placeholder reads `Go crazy` instead. <kbd>ctrl+x</kbd> cancels the agent
you are viewing, and the sidebar shows a highlighted banner with the agent's
handle and live status.

## Review and apply

The easiest route is to ask the main agent, which has the handle and branch
from the result:

```text
Review @tester's diff. If go test ./pkg/x/... passes, apply it; otherwise
dismiss it and tell me why.
```

It calls `apply_dispatch` or `dismiss_dispatch`, each of which asks your
permission first:

| Tool | What it does |
| --- | --- |
| `apply_dispatch` with `mode: merge` (default) | A `--no-ff` merge of the dispatch branch into your current branch, then the workspace and branch are removed |
| `apply_dispatch` with `mode: squash` | `git merge --squash`: the work lands **staged and uncommitted** in your checkout, for you to commit |
| `apply_dispatch` with `mode: cherry-pick` | Cherry-picks the dispatch branch's commits since its base |
| `dismiss_dispatch` | Deletes the worktree, the branch and the registry entry. No undo |

Uncommitted work in the worktree is committed on the dispatch branch first, as
`crush-dispatch <id>: uncommitted work`, so every mode brings it in. Apply
refuses, and changes nothing, when the dispatch is still running (cancel it
first), when your own checkout has uncommitted changes or a merge, rebase or
cherry-pick in progress, and when the work conflicts with yours — the
conflicting paths are listed and the merge is aborted. Neither tool applies
to an [external agent](/agents/configuration#external-agents): it wrote
nothing to your disk.

You can also leave the workspace in place. Quitting keeps every dispatch with
commits or uncommitted changes; see [Clean up](#clean-up).

### By hand, if you must

Find the worktree first. `crush dispatch list` prints each workspace with its
branch, and `git worktree list` prints the absolute path, which is
`<data dir>/worktrees/<repo-key>/crush-dispatch-<id>` (`crush dirs` shows the
data directory; the repo key is a 12-character hash of the repository). The
agent may have left its work uncommitted, so commit it inside the worktree,
then merge from your checkout:

```bash
WT=$(git worktree list | awk '/crush-dispatch-<id>/ { print $1 }')
git -C "$WT" add -A                          # include new files
git -C "$WT" diff --cached main --stat       # everything it changed since main
git -C "$WT" commit -m "test: table-driven tests for the lexer"   # if anything is staged

git diff main...crush-dispatch-<id>          # review
git merge crush-dispatch-<id>                # or: git merge --squash, git cherry-pick
```

Use the branch you dispatched from in place of `main`. Then remove the
worktree before the branch, since git won't delete a branch that's still
checked out:

```bash
git worktree remove "$WT"                    # --force to drop uncommitted work
git branch -D crush-dispatch-<id>
```

Crush's own record of the dispatch still says it is undecided; `crush dispatch
prune --all-dead` clears that once the owning process has exited.

## Clean up

Nothing is discarded behind your back. At exit, Crush removes only the
workspaces you applied or dismissed and the ones that produced no work;
anything with commits or uncommitted changes stays on disk and on its branch.

From the shell, `crush dispatch list` shows every workspace and whether its
owning process is still alive, and `crush dispatch prune` removes the decided
ones whose owner is gone. `--all-dead` widens that to every dead owner's
workspace, skipping ones that hold changes unless you add `--force`;
`--dry-run` prints the plan and deletes nothing.

## Revisit later

The agent's session outlives its worktree. To read a past agent's transcript:

1. Press <kbd>ctrl+s</kbd>. A session with sub-agents shows `▸N` next to its
   timestamp.
2. Select it and press <kbd>ctrl+]</kbd> to list its sub-agent sessions.
   (<kbd>enter</kbd> on the session opens the session itself.)
3. Select a sub-agent and press <kbd>enter</kbd>. Crush loads the parent if
   needed and opens the sub-agent in inspect mode, read-only.

Sub-agent sessions can't be continued; dispatch a new agent instead. A
finished agent's block keeps its terminal state across restarts: the result
is stamped on the parent's `dispatch_agent` tool call when it is delivered.

[Handles and inspect mode](/agents/handles-and-inspect) is the full reference
for everything on this page.
