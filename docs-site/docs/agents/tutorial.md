---
id: tutorial
title: Your first parallel task
sidebar_label: Tutorial
sidebar_position: 2
description: Dispatch a background agent, watch it, steer it, read its transcript, and merge its work — end to end.
---

# Your first parallel task

:::info[Fork feature]
Multi-agent dispatch is an addition in the `joestump-agent/crush` fork. Start
with the [overview](/agents/overview) if you haven't read it.
:::

The main agent fixes a bug while a dispatched agent writes tests in parallel.
You'll steer the agent, ask about it, read its transcript, and merge its work.

## Before you start

1. **Start Crush from the repository root.** Crush creates dispatch worktrees
   under `.crush/worktrees` in the directory you launched it from. At the root,
   Crush's own `.crush/.gitignore` covers them. From a subdirectory nothing
   does, and `git add -A` stages the worktree as an embedded repository
   ([#383](https://github.com/joestump-agent/crush/issues/383)).
2. **Commit what the agent should see.** A dispatched agent starts from your
   current branch's last commit, not from your working tree.
3. **Decide how its permission prompts get answered.** The dispatched agent's
   `bash`, `edit`, and `write` requests appear in your usual permission dialog,
   but only while the main agent's turn is still running.

:::warning[Known issue]
Once the main agent's turn ends, a dispatched agent's permission request waits
forever and the agent stalls ([#371](https://github.com/joestump-agent/crush/issues/371)). Until that's fixed, do one of these:

- Start Crush with `crush --yolo`, which auto-approves the main agent too. The
  <kbd>ctrl+y</kbd> toggle doesn't reach dispatched agents ([#378](https://github.com/joestump-agent/crush/issues/378)).
- Pre-allow the tools the agent needs, for example
  `permissions allow bash edit multiedit write`. This allowlist applies to the
  main agent too. Dispatched agents read your global config and the project
  config *committed* on their branch; uncommitted edits to the project's
  `crushrc` are not seen. See
  [Allowing tools](/configuration/permissions#allowing-tools).
:::

:::warning[Known issue]
Dispatches that run longer than 3 minutes are reported as failed ([#344](https://github.com/joestump-agent/crush/issues/344)). Size
the task so it can finish in that time: one package, one kind of change.
:::

## Ask for parallel work

Ask for the split in plain language. Name a role, and pass along any
conventions, because the dispatched agent doesn't see `AGENTS.md` ([#386](https://github.com/joestump-agent/crush/issues/386)).

```text
Fix the off-by-one in pkg/x/parse.go yourself. In parallel, dispatch a
"tester" to add table-driven tests for pkg/x/lexer.go. It should use
testify's require package, and run only `go test ./pkg/x/...`.
```

The main agent calls `dispatch_agent` with a self-contained `prompt` and, usually,
a `role` and a `handle`. If it passes no handle, the role is slugged into one
(`docs writer` becomes `docs-writer`). With neither, the handle is `agent`. If
a handle is already taken, the new one gets a numeric suffix: `tester-2`,
`tester-3`. Add "on the large model" to your request if the subtask needs more
than the small model.

The tool returns at once, and the main agent carries on with the parser fix.

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

:::warning[Known issue]
If the main agent is busy when the result arrives, the delivery waits in its
prompt queue. Pressing <kbd>esc</kbd> to clear that queue throws the result
away ([#388](https://github.com/joestump-agent/crush/issues/388)). To recover it, ask *"what did @tester find?"*. The mention
attaches the agent's findings and diff.
:::

## Steer an agent

Start a message with the agent's handle to send it a correction mid-run:

```text
@tester use require.Equal, not assert.Equal
```

The handle must be the very first token, followed by a space. The message goes
to that agent's queue, and the agent picks it up at its next step. Your
message appears on the agent's block on a `→` line, with the agent's reply
beneath it. Nothing lands in the main conversation.

If the agent has already finished, Crush refuses with
`agent @tester finished (completed); task sessions are never continuable — dispatch a new agent instead`.
To follow up, ask the main agent to dispatch a new one.

:::warning[Known issue]
A routed message drops any attachments, and when Crush refuses it, the text
you typed is gone ([#414](https://github.com/joestump-agent/crush/issues/414)). Punctuation glued to the handle (`@tester:` or
`@tester,`) stops it from routing, so the message goes to the main agent
instead ([#415](https://github.com/joestump-agent/crush/issues/415)).
:::

## Ask about an agent

Mention the handle anywhere *after* the first word to ask the main agent about
it:

```text
How far along is @tester and is it covering the empty-input case?
```

Crush attaches the agent's card to your message: handle, role, status, current
todo, and session. For a finished agent, the card adds its findings and diff
summary. The main agent answers from the card. A mention never routes a message
to the agent itself.

The mention must end at a space or the end of a line. `@tester,` or `@tester?`
attaches no card ([#415](https://github.com/joestump-agent/crush/issues/415)).

## Inspect its transcript

To read everything the agent did, including its reasoning, every tool call and
result, and any todo nudges it got:

1. Press <kbd>tab</kbd> to focus the chat, then select the agent block with
   <kbd>shift+↑</kbd> / <kbd>shift+↓</kbd> (or click it).
2. Press <kbd>ctrl+]</kbd>.

The chat window now shows the agent's live transcript, which follows the
stream as the agent works. If no agent block is selected, <kbd>ctrl+]</kbd>
opens the first live agent. Press <kbd>ctrl+]</kbd> again to cycle through the
live agents. Press <kbd>ctrl+[</kbd> to go back; the chat's scroll position is
restored.

While you inspect, the editor still belongs to the main agent: anything you
send goes to the main conversation, as the editor placeholder reminds you:
`Inspecting <title> · ctrl+[ returns · prompts go to the parent`. In yolo mode
the placeholder reads `Go crazy` instead.

:::warning[Known issue]
<kbd>esc</kbd> behaves differently depending on your terminal ([#404](https://github.com/joestump-agent/crush/issues/404)). In
terminals without the kitty keyboard protocol, such as macOS Terminal.app or
tmux, <kbd>ctrl+[</kbd> and <kbd>esc</kbd> are the same key, so
<kbd>esc</kbd> leaves inspect mode. In terminals that support the protocol,
such as kitty, Ghostty, WezTerm, or foot, <kbd>esc</kbd> keeps its chat
meaning: while the main agent is busy, it clears any queued prompts, and a
double press cancels the main agent's turn. Use <kbd>ctrl+[</kbd> to leave
inspect mode.
:::

:::info[Planned]
<kbd>esc</kbd> and <kbd>ctrl+[</kbd> will both leave inspect mode on every
terminal, and <kbd>esc</kbd> will never cancel the main agent from inside
inspect mode ([#404](https://github.com/joestump-agent/crush/issues/404)).
:::

## Review and merge

The easiest route is to ask the main agent, which has the branch name from the
result:

```text
Review @tester's diff. If go test ./pkg/x/... passes, merge it into main and
remove its worktree and branch.
```

To do it by hand, find the worktree first. `git worktree list` shows each
`.crush/worktrees/crush-dispatch-<id>` path with its branch. The agent may have
left its work uncommitted, so commit it inside the worktree, then merge from
your checkout:

```bash
cd .crush/worktrees/crush-dispatch-<id>
git add -A                       # include new files
git diff --cached main --stat    # everything it changed since main
git commit -m "test: table-driven tests for the lexer"   # if anything is staged
cd -

git diff main...crush-dispatch-<id>   # review
git merge crush-dispatch-<id>         # or: git merge --squash, git cherry-pick
```

Use the branch you dispatched from in place of `main`.

## Clean up

Remove the worktree first, then the branch. Git won't delete a branch that's
still checked out in a worktree. Add `--force` to `git worktree remove` to
discard uncommitted work you don't want.

```bash
git worktree remove .crush/worktrees/crush-dispatch-<id>
git branch -D crush-dispatch-<id>
```

:::warning[Known issue]
Crush cannot record your decision about a dispatch yet ([#368](https://github.com/joestump-agent/crush/issues/368)),
so cleanup is still by hand: commit work in the worktree as above and copy the
branch to a name without the prefix, `git branch keep/lexer-tests crush-dispatch-<id>`,
then remove the worktree. Quitting is safe: exit already keeps any dispatch
with commits or uncommitted changes ([#367](https://github.com/joestump-agent/crush/issues/367)).
:::

:::info[Planned]
`apply_dispatch` and `dismiss_dispatch` will record your decision about a
workspace directly ([#368](https://github.com/joestump-agent/crush/issues/368)),
and `crush dispatch list` and `crush dispatch prune` will manage them from the
shell ([#369](https://github.com/joestump-agent/crush/issues/369)).
:::

## Revisit later

The agent's session outlives its worktree. To read a past agent's transcript:

1. Press <kbd>ctrl+s</kbd>. A session with sub-agents shows `▸N` next to its
   timestamp.
2. Select it and press <kbd>ctrl+]</kbd> to list its sub-agent sessions.
   (<kbd>enter</kbd> on the session opens the session itself.)
3. Select a sub-agent and press <kbd>enter</kbd>. Crush loads the parent if
   needed and opens the sub-agent in inspect mode, read-only.

Sub-agent sessions can't be continued; dispatch a new agent instead. After a
restart, a finished agent's block may read `working` again, because the live
registry is gone ([#410](https://github.com/joestump-agent/crush/issues/410)).

:::warning[Known issue]
Two paths can still make a sub-agent session the active session:
`crush -s <sub-agent session id>`, and `crush run --continue` when the most
recently updated session is a sub-agent ([#413](https://github.com/joestump-agent/crush/issues/413)). Open sub-agent sessions
through the sessions picker instead.
:::

[Handles and inspect mode](/agents/handles-and-inspect) is the full reference
for everything on this page.
