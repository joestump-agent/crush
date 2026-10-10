---
id: troubleshooting
title: Multi-agent troubleshooting
sidebar_label: Troubleshooting
description: Symptoms, causes and fixes for dispatched agents, todo enforcement, handles and inspect mode, external agents and the TCP listener, plus the git commands to recover a dispatch worktree by hand.
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
| `Dispatch A2A stream failed` | The transport gave up on a dispatch mid-stream after its resume attempts |
| `Todo enforcement killed the run` | A dispatched agent was killed for ignoring nudges |
| `Dispatch run killed by watchdog` | A dispatch hit `hard_timeout` or `stall_window` |
| `Dispatch workspace sweep failed` | Cleanup at exit stopped partway |
| `Dispatch result dropped: parent session is gone` | Debug level: the result had nowhere to go |
| `External agent definition cannot be dispatched` | A `runtime: a2a` definition is unusable; the `reason` names the field |
| `A2A host listening on TCP` | The TLS listener is up; `addr`, `url` and `mutual_tls` say how |
| `A2A TCP listener not started; serving the unix socket only` | The listener could not bind (port taken, bad certificate); dispatch still works locally |
| `A2A TCP request rejected: unauthenticated` | A TCP caller without a valid credential; `remote` and `reason` (`no credential` or `invalid bearer token`) say who and why |
| `A2A TCP listener has no address to advertise` / `… accepts only this process's bearer token` | Startup warnings about a listener nobody can use; see [Working with other agents](/agents/team#startup-warnings) |

## Dispatch won't start

### "dispatch unavailable: … is not a git repository"

**Cause.** A dispatch runs in a git worktree, so the directory Crush was
launched in must be inside a git repository. Crush remembers the failure for
the rest of the process, so running `git init` afterwards changes nothing
until you restart.

**Fix.** Launch Crush inside the repository (`cd` there, or pass `--cwd`),
make at least one commit, and restart if you just created the repository.

### "dispatch at capacity: N agents are already running (dispatch.max_concurrent=M)"

**Cause.** At most `options.dispatch.max_concurrent` dispatches run at once —
4 by default — counting ones still provisioning. The call provisioned nothing.

**Fix.** Wait for one to finish, cancel one (<kbd>ctrl+x</kbd> on its block,
or "cancel @tester"), or raise the cap: `option dispatch-max-concurrent 8`.
A value below 1 fails the load.

### The main agent has no `dispatch_agent` tool

| Cause | How to tell | Fix |
| --- | --- | --- |
| You are in `crush run` | The five dispatch tools are interactive-only; a non-interactive run exits with the turn, so a dispatch could never report back | Ask for the work directly, or use the TUI |
| The `worker` is disabled and no other dispatch agent is defined | `agent set worker --disabled true` or `"worker": {"disabled": true}` in your config | Enable it, or `agent add` another dispatch agent |
| The tools are denied | `permissions deny dispatch_agent …` or `options.disabled_tools` | Remove the entry |
| You are in plan mode, or asking a sub-agent | Only the main `coder` agent gets the tools | Switch back to the coder |

### "provision dispatch workspace: …"

| The error mentions | Cause | Fix |
| --- | --- | --- |
| `resolve current revision` or `resolve base` | The repository has no commits yet, or the main agent passed a `branch` that does not exist | Commit something, or tell the main agent which branch to use |
| `create worktree` | git refused to create the worktree | Read git's message in the error: a branch named `crush-dispatch-<id>` that already exists, a locked index, a missing data directory |

### "unknown skills: …"

The main agent asked for a skill by a name that matches nothing Crush
discovered. Skills come from your session's skills paths (`option skill-path`,
plus the default `.agents/skills`, `.crush/skills`, `.claude/skills` and
`.cursor/skills`), resolved inside the worktree, and a `dispatch_agent` call's
`skills` argument can only narrow the agent definition's `skills` list, never
widen it. Check the name in the Skills dialog and the definition's list.

### `agent "<id>" cannot be dispatched: agents.<id>.card: …`

**Cause.** The named [external agent](/agents/configuration#external-agents)
is unusable: its card URL is missing, uses plain `http` to a host that is not
loopback, carries credentials, or `auth` is set without a `token`. Such a
definition loads anyway — nothing on the command line tells you — and drops
out of the `agent` enum; this refusal is where you find out. The same text
appears on every default dispatch when `option dispatch-default-agent` names
such an agent.

**Fix.** Correct the field the message names.

### The agent doesn't see my latest changes

That is by design. The worktree is cut from a **committed** revision: your
current branch, or the `branch` the main agent passed. It never includes
uncommitted changes. Commit before you dispatch.

## Running agents

### A dispatched agent stays at working and never progresses

| Cause | How to tell | Fix |
| --- | --- | --- |
| It is waiting on a permission prompt | The permission dialog shows a request prefixed with its `@handle`; the block's `→` line stops moving | Answer the dialog. Toggle yolo with <kbd>ctrl+y</kbd> to approve the rest, or pre-allow with `permissions allow bash edit multiedit write` |
| It asked you a question | Your question prompt shows one labeled with the `@handle` | Answer it, or dismiss it: the agent carries on with "the user declined to answer" |
| A long command is running | Inspect it with <kbd>ctrl+]</kbd>: a `bash` call is open, or it is reading a background job with `job_output` | Wait, or steer it: `@tester stop and report what you have` |
| It is actually working | The transcript keeps moving | Wait |

A run that stalls for good is bounded only by what you configured: `option
dispatch-timeout 30m` kills it after half an hour with reason `hard timeout`;
`option dispatch-stall 5m` kills a run whose todo list stops changing;
`inactivity_timeout` (JSON) fails a served run that emits no events at all.

### Stopping a single dispatched agent

Select the agent's block in the chat — or drill into it with
<kbd>ctrl+]</kbd> — and press <kbd>ctrl+x</kbd>. Asking the main agent to
"cancel @tester" works too: it calls `cancel_dispatch`. The run ends killed
with reason `canceled by user`, the card shows **canceled**, and the workspace
and branch remain so you can review the work or dispatch a fresh agent against
it. A steer (`@tester stop and report`) is the gentler lever — the agent can
wrap up on its own. <kbd>esc</kbd> twice cancels only the main agent's turn.
Quitting Crush cancels every dispatched run with reason `crush exited` and
keeps any workspace that holds work.

### My steer never got a reply

| Cause | How to tell | Fix |
| --- | --- | --- |
| The agent finished first | The dispatch result lists your message under `undelivered_steers` | Dispatch a new agent with the follow-up in its prompt |
| The agent was killed moments later | The block shows **Killed**, and your `→` line never appeared | Same |
| The text went to the main agent | It appears in the main chat as your prompt | The handle was not the very first token, or it was glued to text (`@tester's`). Retype with the handle first |

### The agent asked a question nobody can see

A dispatched agent gets the `question` tool only while your session is
interactive at dispatch time. If you attached from another client, or the
dispatch outlived your TUI, the question is answered automatically with
`no interactive user; proceed with your best judgment` and the agent carries
on.

## Kills and nudges

### A dispatched agent was killed

The block shows **Killed** with one of `ignored nudges`, `stalled todos`,
`hard timeout` or `tool loop`, and the main agent receives the reason, the
agent's last message and the salvageable diff. See
[Wander kill](/agents/todo-enforcement#wander-kill) for what trips each one.

To give agents more slack, raise `option todo-nudge-threshold`, set
`option todo-kill-after-nudges off`, or raise `option dispatch-timeout`. The
workspace is kept either way; re-dispatch with a tighter prompt, or dismiss
it.

### The main agent keeps getting todo reminders

Messages like "You have made tool calls without recording a todo list…" in
the transcript are the nudge ladder. The main agent, plan mode and `agent`
sub-agents are nudged (when they have the `todos` tool) and never killed; a
nudged run gets at most two reminders. Turn the nudges off with
`option todo-nudge false`, or raise `option todo-nudge-threshold`. See
[Todo enforcement](/agents/todo-enforcement#examples).

### The hard gate blocks every edit

`no todo list exists for this session; call the todos tool…` means
`option todo-hard-gate on` (or `hard_gate: true`) is set and the session has
no todo list. The agent writes one and the next mutating call goes through.
Turn the gate off if you did not mean to enable it.

## Results

### The result says "(no changes)" but the agent did commit work

**Cause.** The diff is computed against the merge-base of the dispatch's base
and your `HEAD`, falling back to the base SHA recorded at provision. A
dispatch cut from an unusual base — a detached `HEAD` mid-rebase, or
`branch: "HEAD~1"` — can diff against the wrong point.

**Fix.** Check the branch directly: `git log --oneline -5 crush-dispatch-<id>`
and `git -C <workspace_path> status`. Dispatch from a named branch.

### "(diff unavailable: …)"

The diff could not be captured, usually because a git command failed inside
the worktree. The run itself completed; read the worktree with git directly,
using the `workspace_path` from the result.

### `apply_dispatch` refused

| Message | Fix |
| --- | --- |
| `dispatch <id> is still running; cancel it first` | Cancel it, or wait |
| `cannot apply dispatch <id>: the checkout is dirty; commit or stash your changes first` | Your own checkout has uncommitted changes |
| `… a merge, rebase, or cherry-pick is already in progress (MERGE_HEAD); finish or abort it first` | Finish the operation in your checkout |
| Conflicting paths listed, "the merge is aborted" | Resolve by hand: merge the branch yourself, or re-dispatch against your current branch |
| `dispatch <id> ran on an external agent: nothing was written to disk, so there is no workspace or branch to apply or dismiss` | External agents produce findings only; there is nothing to apply |
| `no dispatch "<ref>" is known; dispatch one first` | The handle or id is wrong, or the dispatch was already applied or dismissed |

### A dispatched agent ignored my config, hooks or deny list

| What was ignored | Why | Fix |
| --- | --- | --- |
| A `crushrc` or `crush.json` committed on the base revision | The dispatched agent reuses your session's config and never reads the worktree's own config files | Put it in the launch directory, or your global config |
| A tool the agent used although you denied it | `permissions deny` applies to dispatched agents; check the spelling against the [tool reference](/reference/tools). A tool added back by the agent definition's `tools.allow` is still removed by your deny list | — |
| A skill or MCP server | The agent definition's `skills` and `mcp.allow` filter what the agent sees; the built-in `worker` has no MCP tools | `agent set worker --mcp <server>` |

## Worktrees and branches

Every dispatch creates a worktree at
`<data dir>/worktrees/<repo-key>/crush-dispatch-<id>` on a branch named
`crush-dispatch-<id>`. The data directory is the one `crush dirs` prints
(`.crush` in the project by default); the repo key is 12 hex characters derived
from the repository's git directory. `<data dir>/worktrees/.gitignore` is `*`,
so the worktrees never appear in `git status`. Crush never merges either one
without being asked.

### Worktrees disappeared after quitting

Crush keeps dispatch work at exit: a graceful shutdown removes only
dispatches you applied or dismissed and ones that produced no work, a crash
removes nothing — the next launch reconciles the same way — and work with
commits or uncommitted changes stays on disk and on its branch either way.
Only this process's own workspaces are ever touched; other Crush instances'
live worktrees are left alone.

| How Crush ends | Dispatch worktrees and branches |
| --- | --- |
| <kbd>ctrl+c</kbd> in the TUI, or `SIGTERM` | Live runs are canceled (`crush exited`), then decided and workless workspaces are removed with their branches; work is kept |
| Crash, `SIGKILL`, `SIGHUP` | Nothing is removed; the next launch reconciles |

If work you wanted is gone anyway — removed by hand, or with
`crush dispatch prune --force` — committed work usually survives as
unreachable commits until git garbage-collects them:

```bash
git fsck --unreachable --no-reflogs | grep commit
git branch rescue-<id> <sha>      # once you've found it with git show <sha>
```

Uncommitted changes in a removed worktree are gone.

### Orphan worktrees and `crush-dispatch-*` branches pile up

Workspaces with work are kept on purpose. From the shell:

```bash
crush dispatch list                    # id, handle, branch, disposition, owner alive?, changes?
crush dispatch list --json
crush dispatch prune --dry-run         # what the default prune would remove
crush dispatch prune                   # dead-owner workspaces that were applied or dismissed
crush dispatch prune --all-dead        # every dead owner's workspace, keeping ones with changes
crush dispatch prune --all-dead --force   # those too
```

A workspace whose owning process is still alive is never removed
(`skipped …: owner is alive`), and neither is one without a lock file
(`no lock file; ownership cannot be proven`). A workspace an agent is still
using shows `Owner: alive`; a workspace removed by hand with git may leave a
stale `crush-dispatch-<id>.lock` behind in the worktrees directory, which is
harmless.

### Recover a dispatch by hand

Run these from anywhere in the repository. `git worktree list` prints absolute
paths, so use those.

```bash
# Find them.
crush dispatch list
git worktree list | grep crush-dispatch-
git branch --list 'crush-dispatch-*'

# Inspect one. A dispatched agent often leaves its work uncommitted.
WT=$(git worktree list | awk '/crush-dispatch-<id>/ { print $1 }')
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

Prefer `apply_dispatch` and `dismiss_dispatch` from the session when the
dispatch is still known to it: they do the same steps and record the
decision, so exit and `crush dispatch prune` know the workspace is settled.

To clear every dispatch at once, after reviewing them, and only while no Crush
instance has a live dispatch in this repository:

```bash
crush dispatch prune --all-dead --force
```

## Handles and inspect

### "@tester …" went to the main agent

A leading `@handle` routes your message to that agent only when all of these
hold:

- the handle is the **very first token** of the first line. A mention later in
  the sentence attaches an agent card instead
  ([details](/agents/handles-and-inspect));
- the token ends at whitespace, the end of the message, or one of `: , . ? !`
  followed by whitespace. `@tester's work` is prose;
- the agent was dispatched from **this** session and is live in this
  process. The <kbd>@</kbd> completions list only live agents, so check the
  spelling there. Handles from an earlier run do not carry over.

### "agent @tester finished (completed); task sessions are never continuable"

A finished dispatch (completed, failed or killed) cannot take more messages.
Your message was not sent anywhere and is still in the editor; clear it, then
ask the main agent to dispatch a new agent with the follow-up.

### "steering external agents is not supported yet"

An [external agent](/agents/configuration#external-agents) cannot be steered:
A2A gives a third-party agent no contract for folding a message into a
running task. Cancel it and dispatch again with the new instructions.

### `ctrl+]` says "No live sub-agents to inspect"

Nothing is running, and no agent block is selected. To read a finished agent's
transcript, select its block first (<kbd>tab</kbd>, then <kbd>shift+↑</kbd> /
<kbd>shift+↓</kbd>) and press <kbd>ctrl+]</kbd>, or open it from the sessions
tree (<kbd>ctrl+s</kbd>, <kbd>ctrl+]</kbd> on the parent session).

## External agents

Every failure the main agent sees starts
`dispatch to external agent "<id>" failed:`. The rest is fixed text — nothing
the remote sent — and the full list is in
[External agents → Failures](/agents/configuration#failures). The common
ones:

| Text | Cause | Fix |
| --- | --- | --- |
| `a2a: resolve agent card <url>: HTTP status 401` (or `403`, `404`) | The card URL needs a credential Crush does not send, or is wrong | Serve the card unauthenticated at `/.well-known/agent-card.json`; the bearer token is sent only to the JSON-RPC calls |
| `… TLS certificate verification failed` | The server's certificate does not chain to a CA your system trusts | Install the team CA in the system trust store, or use a publicly trusted certificate. Crush has no per-agent CA setting |
| `… the host name did not resolve` / `connection refused` / `timed out` | Network | — |
| `a2a: agent card <url> declares no HTTP bearer security scheme; refusing to send the configured bearer token` | You set `auth`, but the card has no `http`/`bearer` security scheme | Drop `auth`, or fix the card |
| `a2a: agent card <url> names its JSON-RPC service on another origin than <origin>; refusing it …` | The card's interface URL is on a different host or port than the card | Fetch the card from the origin that serves the agent |
| `a2a: agent card <url> offers no JSON-RPC interface on a protocol version crush speaks (1.x)` | The card offers only gRPC or HTTP+JSON, or a 0.x version | Crush speaks JSON-RPC 1.x only |
| `the external agent asked for input; crush does not forward …` | The remote paused on `input-required` or `auth-required` | Re-dispatch with a self-contained prompt |
| `agent "<id>": auth.token did not resolve; the log has the reason` | The `$VAR` is unset or the `$(cmd)` failed | Read `crush logs`; the command's stderr is there |

## The TCP listener

### Crush fails to load with `a2a.listen requires tls_cert and tls_key`

Plain TCP is refused. Set `option a2a-tls-cert` and `option a2a-tls-key` as
well, or drop `option a2a-listen`. A relative certificate path fails too
(`… is a relative path; use an absolute path or one starting with ~/`).

### The log says `A2A TCP listener not started; serving the unix socket only`

The port is taken, or the certificate and key did not load as a pair. Dispatch
keeps working over the socket. Fix the setting and restart; the listener does
not reload.

### A teammate gets `401 a2a: unauthenticated`

Your log has `A2A TCP request rejected: unauthenticated` with their address
and a `reason`:

- `no credential`: they sent neither a client certificate nor a bearer
  token. Without `option a2a-client-ca`, only your own process's token is
  accepted, and it never leaves the process — so a remote caller cannot
  authenticate at all. Set `a2a-client-ca` and issue them a certificate
  signed by it; see [Working with other agents](/agents/team).
- `invalid bearer token`: they sent a token, but not yours. A bearer token is
  for a process's own clients; teammates use certificates.

### A teammate's call is rejected with `no running agent for context …`

They authenticated and reached a definition route (`/agents/worker`), but
those routes run nothing for a remote caller yet: a Crush host accepts the
call and rejects the task until the peer registry
([#334](https://github.com/joestump-agent/crush/issues/334)) lands. This is
the expected answer, and the proof that the certificates and routes line up.
See [what works today](/agents/team#what-works-today).

### A teammate gets `404` for `/agents/worker/.well-known/agent-card.json`

The host builds a card for every agent definition but does not serve it on
the wire: its router answers `404` for any sub-path of a route. Nothing is
misconfigured; the card is not fetchable until the peer registry publishes
it. The same `404` is what a `--card` pointed at a Crush gets.

### `400` or `415` from the listener

`400`: the request's `Host` header matched neither the listen address nor a
name in the server certificate. Dial the listener by a name or address that is
a DNS or IP SAN of `a2a-tls-cert`, or by the configured listen address.
`415`: the request had no `Content-Type: application/json`; the host requires
it on every request, `GET` included.

## Client/server mode

With `CRUSH_CLIENT_SERVER=1`, the agent block, @ completions, steering,
cards and <kbd>ctrl+x</kbd> all work through the server's A2A proxy. A card
for a finished agent shows the agent's final status text until the server has
stamped the result on the dispatch record, then its findings. If the agent
index returns `503`, the workspace's A2A host has not started yet; it starts
with the first session that needs it.
