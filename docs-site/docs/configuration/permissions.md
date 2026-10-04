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

## Denying tools

```bash
permissions deny bash sourcegraph
```

Denied tools do not appear in the model's tool list at all, so the model never
tries to call them — with one current exception, inside
[dispatched agents](#dispatched-agents). To disable tools from a specific MCP
server instead, use `--disabled-tools` / `--enabled-tools` on the server — see
[MCP](/features/mcp#restricting-tools).

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

Dispatch is on by default. Turn it off by denying both of its tools:

```bash
permissions deny dispatch_agent message_agent
```

:::warning[Known issue]
- A deny list does not hold inside a dispatched agent. `permissions deny` and
  `options.disabled_tools` can hide `bash`, `edit`, `multiedit`, `write`, and
  `todos` from the main agent, but a dispatched agent always gets them back.
  If you rely on a deny list, deny `dispatch_agent` as well. Tracked as
  [#376](https://github.com/joestump-agent/crush/issues/376).
- A dispatched agent follows the yolo setting Crush **started** with, not the
  <kbd>ctrl+y</kbd> toggle. Start with `--yolo`, toggle it off, and dispatched
  agents still approve everything without asking. Tracked as [#378](https://github.com/joestump-agent/crush/issues/378).
- [Hooks](/features/hooks#pretooluse) do not run inside dispatched agents, so
  a hook policy does not cover them. Tracked as [#377](https://github.com/joestump-agent/crush/issues/377).
:::

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
