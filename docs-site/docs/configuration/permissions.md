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
tries to call them. To disable tools from a specific MCP server instead, use
`--disabled-tools` / `--enabled-tools` on the server — see
[MCP](/features/mcp#restricting-tools).

## Blocked commands

The `bash` tool blocks a set of potentially dangerous commands by default — for
example `ssh`, `curl`, `systemctl`, and various package managers. Selectively
remove commands from that blocklist:

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
