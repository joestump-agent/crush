---
id: troubleshooting
title: Multi-agent troubleshooting
sidebar_label: Troubleshooting
description: Symptoms, causes and fixes for dispatched agents, todo enforcement, handles and inspect mode, plus the git commands to recover dispatch worktrees by hand.
---

# Multi-agent troubleshooting

:::info[Fork feature]
Everything on this page concerns [dispatched agents](/agents/overview) and
[todo enforcement](/agents/todo-enforcement), which exist only in this fork.
Report problems to
[joestump-agent/crush](https://github.com/joestump-agent/crush/issues).
:::

Each entry gives the symptom, its cause and the fix. Most problems leave a line
in the [log](/reference/logging) (`crush logs --tail 200`):

| Log message | Meaning |
| --- | --- |
| `Dispatch A2A stream failed` | The transport gave up on a dispatch mid-stream |
| `Dispatch A2A server failed to start` | The dispatch fell back to running in-process |
| `Todo enforcement killed the run` | An agent was killed for ignoring nudges |
| `Dispatch run killed by watchdog` | A dispatch hit `hard_timeout` (or `stall_window`) |
| `Dispatch workspace sweep failed` | Cleanup at exit stopped partway |
| `Dispatch result dropped: parent session is gone` | Debug level: the result had nowhere to go |

## Dispatch won't start

### "dispatch unavailable: … is not a git repository"

**Cause.** A dispatch runs in a git worktree, so the directory Crush was
launched in must be inside a git repository. Crush remembers the failure for
the rest of the process, so running `git init` afterwards changes nothing
until you restart.

**Fix.** Launch Crush inside the repository (`cd` there, or pass `--cwd`),
make at least one commit, and restart if you just created the repository.

### "provision dispatch workspace: …"

| The error mentions | Cause | Fix |
| --- | --- | --- |
| `resolve current revision` or `resolve base` | The repository has no commits yet, or the main agent passed a `branch` that does not exist | Commit something, or tell the main agent which branch to use |
| `create worktree` | git refused to create the worktree | Read git's message in the error. If several agents were dispatched at once, retry them one at a time: parallel provisioning races on `.git/config` ([#381](https://github.com/joestump-agent/crush/issues/381)) |

### "unknown skills: …"

The main agent asked for a skill the worktree cannot see. A dispatched agent
discovers skills from the **committed** checkout and your global config, so
skills in gitignored or uncommitted directories do not exist for it. Commit
the skill, or move it to a global skills path.

### The agent doesn't see my latest changes

That is by design. The worktree is cut from a **committed** revision: your
current branch, or the `branch` the main agent passed. It never includes
uncommitted changes. Commit before you dispatch.

## Running agents

### A dispatched agent stays at running and never progresses

| Cause | How to tell | Fix |
| --- | --- | --- |
| It is waiting on a permission request that will never be shown ([#371](https://github.com/joestump-agent/crush/issues/371)) | You did not start Crush with `--yolo`, and the main agent's turn has already ended | Start Crush with `--yolo`, or auto-approve the tools it needs in a config the worktree reads. See [Permissions and yolo](/agents/configuration#permissions-and-yolo) |
| It was killed, but the kill never reached the transport ([#342](https://github.com/joestump-agent/crush/issues/342)) | The log has `Todo enforcement killed the run` or `Dispatch run killed by watchdog` | Nothing to do: the block flips to **failed** with the kill reason once the kill lands |
| It is actually working | Focus the block and press <kbd>ctrl+]</kbd> to watch its transcript | Wait, or steer it with `@handle …` |

A killed or canceled run resolves to **failed** with its reason; there is no
fixed deadline that ends a running dispatch.

### Stopping a single dispatched agent

Select the agent's block in the chat — or drill into it with
<kbd>ctrl+]</kbd> — and press <kbd>ctrl+x</kbd>
([#373](https://github.com/joestump-agent/crush/issues/373)). Asking the main
agent to "cancel @tester" works too: it calls `cancel_dispatch`. The run ends
killed with reason "canceled by user", the card shows **canceled**, and the
workspace and branch remain so you can review the work or dispatch a fresh
agent against it. A steer (`@handle stop and report`) is still the gentler
lever — the agent can wrap up on its own. <kbd>esc</kbd> twice cancels only
the main agent's turn, and quitting Crush ends every dispatched run; read
[Worktrees disappeared](#worktrees-disappeared-after-quitting) first.

### Dispatched tests never report back

`bash` moves any command that runs longer than 60 seconds into a background
job and tells the model to read it with `job_output`. Dispatched agents do not
have `job_output` or `job_kill` ([#384](https://github.com/joestump-agent/crush/issues/384)), so a long test run's output never
reaches them. Background jobs also outlive the agent that started them: they
keep running in the worktree after a kill or teardown ([#385](https://github.com/joestump-agent/crush/issues/385)). Ask for test
commands that finish within a minute. Stop leftover jobs with your usual
process tools.

## Kills and nudges

### My main agent, plan mode or a sub-agent stopped after todo reminders

The transcript shows one or two messages like "You have made tool calls without
recording a todo list…" as if you had sent them. Then the turn ends as if you
had canceled it, and queued prompts are gone. In plan mode or an `agent`-tool
sub-agent, the run stops after about a dozen tool calls. The log has
`Todo enforcement killed the run`.

**Cause.** The default ladder kills every agent that ignores two nudges, not
only dispatched ones ([#393](https://github.com/joestump-agent/crush/issues/393)). Plan and task agents have no `todos` tool, so
they cannot comply at all ([#394](https://github.com/joestump-agent/crush/issues/394)).

**Fix.** Turn off the kill (JSON only):

```json
{ "options": { "todo_enforcement": { "kill_after_nudges": 0 } } }
```

Add `"enabled": false` to stop the nudges as well. See
[Todo enforcement](/agents/todo-enforcement#examples).

### A dispatched agent was killed

When a kill is reported, the block shows **Killed** with one of
`ignored nudges`, `stalled todos`, `hard timeout` or `tool loop`. The main agent
receives the reason, the agent's last message and the salvageable diff. See
[Wander kill](/agents/todo-enforcement#wander-kill) for what trips each one.

On current main, kills of transported dispatches usually show as **failed**
after 3 minutes instead ([#342](https://github.com/joestump-agent/crush/issues/342), [#343](https://github.com/joestump-agent/crush/issues/343)). To give agents more slack, raise
`nudge_threshold` or set `kill_after_nudges` to `0`. A `kill_after_nudges`
above `2` behaves exactly like `2` ([#401](https://github.com/joestump-agent/crush/issues/401)).

## Results

### The result says "(no changes)" but the agent did commit work

**Cause.** When the dispatch base is `HEAD` or relative (a detached parent
during a rebase or bisect, a jj-colocated repository, or `branch: "HEAD~1"`),
the diff is computed against the worktree's own `HEAD`. The agent's commits
drop out of the diff ([#380](https://github.com/joestump-agent/crush/issues/380)).

**Fix.** Check the branch directly: `git log --oneline -5 crush-dispatch-<id>`
and `git -C <workspace_path> status`. Dispatch from a named branch to avoid it.

### The diff summary is garbled

Escape codes or `EXTERNAL-DIFF` lines in the diff come from your git config:
`color.ui = always` or `diff.external`. Crush also inherits `GIT_DIR` and
`GIT_INDEX_FILE` from its environment, for example when launched from a git
hook. Dispatch git commands are not isolated from either ([#382](https://github.com/joestump-agent/crush/issues/382)). Set
`color.ui` to `auto`, launch Crush without those variables, and read the
worktree with git directly. The agent's actual work is unaffected.

### A dispatched agent ignored my config, hooks or deny list

| What was ignored | Why | Fix |
| --- | --- | --- |
| A `.crushrc` or `crush.json` committed on the base revision | The dispatched agent reuses the launch directory's config and never reads the worktree's own config files ([#374](https://github.com/joestump-agent/crush/issues/374)) | Put it in the launch directory, or your global config |
| A `PreToolUse` hook | Hooks fire on the `dispatch_agent` call, not inside the dispatched agent ([#377](https://github.com/joestump-agent/crush/issues/377)) | Gate dispatch itself with a hook on `^dispatch_agent$` |
| `permissions deny bash` (or `edit`, `write`…) | Dispatched agents always get `bash`, `edit`, `multiedit`, `write` and `todos` ([#376](https://github.com/joestump-agent/crush/issues/376)) | Deny `dispatch_agent` itself |
| Turning yolo off with <kbd>ctrl+y</kbd> | Dispatched agents follow the `--yolo` startup flag ([#378](https://github.com/joestump-agent/crush/issues/378)) | Restart without `--yolo` |


## Worktrees and branches

Every dispatch creates a worktree at
`<launch directory>/.crush/worktrees/crush-dispatch-<id>` on a branch named
`crush-dispatch-<id>`. Crush never merges either one.

### Worktrees disappeared after quitting

Crush keeps dispatch work at exit ([#367](https://github.com/joestump-agent/crush/issues/367), [#365](https://github.com/joestump-agent/crush/issues/365)): a graceful shutdown removes only
dispatches you applied or dismissed and ones that produced no work, a crash
removes nothing — the next launch reconciles the same way — and work with
commits or uncommitted changes stays on disk and on its branch either way.
Only this process's own workspaces are ever touched; other Crush instances'
live worktrees are left alone.

| How Crush ends | Dispatch worktrees and branches |
| --- | --- |
| <kbd>ctrl+c</kbd> in the TUI, or `SIGTERM` | Released synchronously at shutdown: decided and workless workspaces are removed with their branches; work is kept for salvage ([#367](https://github.com/joestump-agent/crush/issues/367)) |
| `SIGINT` (`kill -INT`, or <kbd>ctrl+c</kbd> in `crush run`) | Nothing is removed; the next launch reconciles ([#367](https://github.com/joestump-agent/crush/issues/367), [#365](https://github.com/joestump-agent/crush/issues/365)) |
| Crash, `SIGKILL`, `SIGHUP` | Nothing is removed; the next launch reconciles |

If work you wanted is gone anyway — removed by hand, or by an older Crush —
committed work usually survives as unreachable commits until git
garbage-collects them:

```bash
git fsck --unreachable --no-reflogs | grep commit
git branch rescue-<id> <sha>      # once you've found it with git show <sha>
```

Uncommitted changes in a removed worktree are gone.

:::info[Planned]
You'll apply or dismiss each dispatch explicitly ([#368](https://github.com/joestump-agent/crush/issues/368)) and manage
workspaces from the shell with `crush dispatch list` and `crush dispatch
prune` ([#369](https://github.com/joestump-agent/crush/issues/369)).
:::

### Orphan worktrees and `crush-dispatch-*` branches pile up

Normal quits usually leave worktrees behind (see above), and so do crashes. A
failed parallel dispatch can leave a branch with no worktree, which the sweep
never finds ([#381](https://github.com/joestump-agent/crush/issues/381)). Each orphan is a full checkout, so clean them up by
hand.

### Recover a dispatch by hand

Run these from anywhere in the repository. `git worktree list` prints absolute
paths, so use those.

```bash
# Find them.
git worktree list | grep crush-dispatch-
git branch --list 'crush-dispatch-*'

# Inspect one. A dispatched agent often leaves its work uncommitted.
WT=/path/to/project/.crush/worktrees/crush-dispatch-<id>
git -C "$WT" status --short
git -C "$WT" log --oneline main..HEAD     # use the branch it was cut from

# Keep uncommitted work: commit it inside the worktree first.
git -C "$WT" add -A
git -C "$WT" commit -m "Salvage dispatch <id>"

# Review, then bring it in.
git diff main...crush-dispatch-<id>
git merge crush-dispatch-<id>             # or: git cherry-pick main..crush-dispatch-<id>

# Remove it.
git worktree remove --force "$WT"
git branch -D crush-dispatch-<id>
git worktree prune
```

To clear every dispatch at once, after reviewing them, and only while no Crush
instance in this directory has a live dispatch:

```bash
git worktree list --porcelain \
  | awk '/^worktree .*crush-dispatch-/ { sub(/^worktree /, ""); print }' \
  | xargs -I{} git worktree remove --force {}
git branch --list 'crush-dispatch-*' --format='%(refname:short)' \
  | xargs -I{} git branch -D {}
git worktree prune
```

### `.crush/` shows up in `git status`

**Cause.** Worktrees always go under `<launch directory>/.crush/worktrees`,
whatever your `data_directory` is ([#383](https://github.com/joestump-agent/crush/issues/383)). Crush's own `.gitignore` covers
that directory only when it is also your data directory. That is the case when
you launch from the repository root with default settings, but not when:

- you launch from a subdirectory. You get `<subdir>/.crush/worktrees`. If you
  normally run Crush from the root, the next launch from that subdirectory
  picks `<subdir>/.crush` as its data directory, and your sessions seem to
  vanish;
- you set a custom `data_directory`.

Typing <kbd>@</kbd> in the editor creates the directory even if you never
dispatch. Don't `git add -A` while it is there: git stages each worktree as an
embedded repository.

**Fix.** Launch Crush from the repository root, and add `.crush/` to your
`.gitignore`. Remove a stray `<subdir>/.crush` once its worktrees are cleaned
up.

## Handles and inspect

### "@tester …" went to the main agent

A leading `@handle` routes your message to that agent only when all of these
hold:

- Crush runs in-process. With `CRUSH_CLIENT_SERVER=1`, handles resolve to
  nothing, steering is unavailable, and the message goes to the main agent as
  a normal prompt;
- the handle is the **very first token** of the first line. A mention later in
  the sentence attaches an agent card instead
  ([details](/agents/handles-and-inspect));
- whitespace or the end of the message follows it. `@tester, stop` does not
  route;
- the agent is live in **this** process. The <kbd>@</kbd> completions list only
  live agents, so check the spelling there. Handles from an earlier run do not
  carry over.

### "agent @tester finished (completed); task sessions are never continuable"

A finished dispatch (completed, failed or killed) cannot take more messages.
Your message was not sent anywhere, and any attachments were dropped. Ask the
main agent to dispatch a new agent and retype the message.

### Esc in inspect mode cancels the main agent

On terminals that implement the kitty keyboard protocol (Ghostty, kitty,
WezTerm and others), <kbd>ctrl+[</kbd> and <kbd>esc</kbd> are different keys.
There <kbd>esc</kbd> keeps its normal meaning even while you inspect a
sub-agent. While the main agent is busy, the first press clears queued prompts
or arms a cancel, and a second press within two seconds cancels the **main**
agent's turn.

Leave inspect mode with <kbd>ctrl+[</kbd>. On terminals that cannot tell the
two keys apart, such as macOS Terminal, <kbd>esc</kbd> in inspect mode already
goes back to the chat.

:::warning[Known issue]
The decided fix makes <kbd>esc</kbd> always leave inspect mode and never cancel
from there. Tracked in [#404](https://github.com/joestump-agent/crush/issues/404).
:::

## Dispatching from `crush run`

`crush run` exits when the main agent's turn ends, and it does not wait for
dispatched agents. They are stopped with the process, their results are never
delivered, and their worktrees are left behind (or removed, if you pressed
<kbd>ctrl+c</kbd>). In scripted runs, ask for the work directly instead of
dispatching it, or deny `dispatch_agent` in the config those runs use.
