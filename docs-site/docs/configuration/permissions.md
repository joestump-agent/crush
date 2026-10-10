---
id: permissions
title: Permissions and safety
sidebar_position: 5
description: Auto-approving tools, hiding tools from the agent, the bash blocklist, and yolo mode.
---

# Permissions and safety

By default Crush asks before every tool call that touches your machine. There
are three separate controls, and they do different things:

| Control | What it does |
| --- | --- |
| **Allow** | Skip the permission prompt for a tool. The agent can still call it. |
| **Deny** | Hide the tool from the agent entirely. It cannot be called. |
| **Blocked commands** | A hard filter *in front of* the `bash` tool's permission flow. |

## Allowing tools

```bash
permissions allow view ls grep edit mcp_context7_get-library-doc
```

MCP tools use their full name, `mcp_<server>_<tool>`.

Use this with care — an allowed `edit` means Crush rewrites files without
asking.

### Read-only bash commands

The `bash` tool runs a small set of read-only commands — `ls`, `pwd`, `which`,
`git status`, `git log`, `git diff`, and a few more — without prompting, even
when `bash` is not in the allow list. Crush parses the command rather than
matching its text, and auto-approves only when the whole command is provably
inert:

- Every statement must be a plain command: no pipeline, redirection,
  backgrounding, subshell, loop or function.
- Every argument must be literal. `$(...)`, backticks, `$VAR`, `$((...))` and
  `<(...)` are rejected rather than evaluated.
- A wrapper that runs another program (`env`, `nice`, `nohup`, `timeout`,
  `time`) is peeled and the inner command is checked on its own, so
  `timeout 5 ls` runs without a prompt and `timeout 5 rm -rf x` does not.
- A read-only command that mutates under a flag or operand prompts in that
  form: `git branch` lists, `git branch -D main` prompts; `git config --get`
  reads, `git config user.name x` prompts.

Anything the parser cannot prove inert falls back to the normal permission
prompt. A sequence of read-only statements (`ls; pwd`) is itself read-only.

## Denying tools

```bash
permissions deny bash sourcegraph
```

Denied tools do not appear in the model's tool list at all, so the model never
tries to call them — inside dispatched agents too. To disable tools from a
specific MCP server instead, use `--disabled-tools` / `--enabled-tools` on the
server — see [MCP](/features/mcp#restricting-tools).

## Blocked commands

The `bash` tool blocks a set of potentially dangerous commands by default — for
example `ssh`, `curl`, `systemctl`, and various package managers.

:::info[Fork feature]
The blocklist itself is upstream. Selectively re-allowing commands —
`allowed_commands`, `allow_all_commands`, `--allow-commands`,
`--allow-all-commands`, `CRUSH_ALLOW_COMMANDS`, and `CRUSH_ALLOW_ALL_COMMANDS`
— is an addition in the `joestump-agent/crush` fork. Upstream the blocklist is
all-or-nothing.
:::

Selectively remove commands from that blocklist:

```json
{
  "$schema": "https://charm.land/crush.json",
  "options": {
    "allowed_commands": ["ssh", "curl", "scp"]
  }
}
```

Or for a single session, via flags or environment variables (flags win):

```bash
# Allow specific commands (repeatable flag, or a comma-separated env var).
crush --allow-commands ssh --allow-commands curl
CRUSH_ALLOW_COMMANDS="ssh,curl,scp" crush

# Remove every command restriction (dangerous).
crush --allow-all-commands
CRUSH_ALLOW_ALL_COMMANDS=1 crush
```

Two things that trip people up:

1. `allowed_commands` only removes commands from the **exact-command**
   blocklist. It does *not* unlock the package-manager argument blocks such as
   `apt install` or `npm -g`. Use `allow_all_commands` (or
   `--allow-all-commands`) for those.
2. **Allowing a command does not auto-approve it.** The blocklist is a hard
   filter in front of the normal permission flow; an allowed command still gets
   the usual permission prompt unless you also enable yolo mode.

## Yolo mode

```bash
crush --yolo
```

Skips every permission prompt. Toggle it mid-session with <kbd>ctrl+y</kbd>.

Be very, very careful with this. Combined with `--allow-all-commands` it means
an LLM can run anything on your machine without asking.

:::warning
In a [shared workspace](/features/server-and-workspaces), `--yolo` and
`--debug` follow a **first-wins** rule. The first client to create a workspace
fixes them; later clients arriving at the same `--cwd` with different values do
not change the running workspace.
:::

## Hooks as a permission layer

[Hooks](/features/hooks) run **before** the permission check, which makes them
the right tool for policy you want enforced deterministically rather than
approved by hand:

- Block `rm -rf` or `git push -f` outright.
- Auto-approve read-only `bash` commands so you stop clicking through them.
- Rewrite tool input before it runs.

## Dispatched agents

:::info[Fork feature]
[Multi-agent dispatch](/agents/overview) is an addition in the
`joestump-agent/crush` fork.
:::

A dispatched agent's permission requests surface in the same prompt as the
main agent's. Its own `permissions.allowed_tools` come from the config in its
worktree — your global config plus the project config committed at the
revision it was cut from; an uncommitted project config does not apply. Your
`--allow-commands` and `--allow-all-commands` overrides carry over.

Your deny list holds inside a dispatched agent: `permissions deny` and
`options.disabled_tools` remove the tool from dispatched agents as well.
Denying all of `bash`, `edit`, `multiedit`, and `write` disables dispatch
entirely.

Dispatch is on by default. Turn it off by denying its five tools, or by
disabling the `worker` agent definition:

```bash
permissions deny dispatch_agent message_agent cancel_dispatch apply_dispatch dismiss_dispatch
# or
agent set worker --disabled true
```

A dispatched agent follows your **live** yolo setting — the <kbd>ctrl+y</kbd>
toggle applies to its next request — and its own tool calls run through your
`PreToolUse` [hooks](/features/hooks#pretooluse), so a hook policy covers it.
See [Permissions and yolo](/agents/configuration#permissions-and-yolo).

## Disabling skills

Hide a skill from the agent entirely, built-in or from disk:

```bash
option disable-skill crush-config
```

See [Skills](/features/skills#disabling-skills).

## Config is trusted code

Both `crushrc` and `crush.json` execute with your privileges before the UI
appears. Don't launch Crush in a directory whose config you haven't reviewed,
and don't `source` configs from the internet.
