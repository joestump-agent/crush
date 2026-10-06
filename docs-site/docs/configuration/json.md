---
id: json
title: Legacy JSON config
sidebar_label: crush.json (JSON)
sidebar_position: 3
description: The original crush.json format — still supported, now deprecated, and where each crushrc builtin maps onto it.
---

# Legacy JSON config

`crush.json` is the original config format. It is **deprecated but fully
supported** — Crush will keep reading it for the foreseeable future, but new
configuration options are only added to [`crushrc`](/configuration/crushrc).

A few things — [hooks](/features/hooks) and
[channel reply routing](/features/channels#reply-routing) among them — still
have richer JSON documentation than Bash documentation, so you will see JSON in
those pages. Both formats are discovered together and deep-merged.

## Shape

```jsonc
{
  "$schema": "https://charm.land/crush.json",
  "providers": {
    "anthropic": { "api_key": "$ANTHROPIC_API_KEY" }
  },
  "models": {
    "large": { "provider": "anthropic", "model": "claude-sonnet-4-20250514" }
  },
  "permissions": { "allowed_tools": ["view", "ls", "grep"] }
}
```

The full field-by-field reference is the JSON schema itself, which is generated
from the Go config types:

```bash
crush schema > schema.json
```

Or read the committed copy at
[`schema.json`](https://github.com/joestump-agent/crush/blob/main/schema.json).
Adding the `$schema` line above gets you completion and validation in any
editor with JSON schema support.

## Where it lives

Same directories as `crushrc`, using `.crush.json` / `crush.json`:

| Priority | Unix-like | Windows |
| --- | --- | --- |
| 1 | `./.crush.json` | `.\.crush.json` |
| 2 | `./crush.json` | `.\crush.json` |
| 3 | `~/.config/crush/crush.json` | `%USERPROFILE%\.config\crush\crush.json` |

If a folder has both a `crushrc` and a `crush.json`, they merge, the `crushrc`
wins on conflicts, and Crush logs a warning.

:::warning[Don't confuse it with state]
`~/.local/share/crush/crush.json` (`%LOCALAPPDATA%\crush\crush.json` on Windows)
is **application state**, not configuration. Crush owns it; don't edit it.
:::

## Shell expansion

In JSON, only selected string fields are shell-expanded at load time: API keys,
URLs, MCP/LSP commands and args, and headers. In `crushrc` there is no such
list — it is all just Bash.

Provider `extra_body` is a non-expanding JSON passthrough. Put env-driven values
in `extra_headers`, `api_key`, or `base_url`.

:::warning
Both formats are trusted code. Any `$(...)` in `crush.json` runs at load time
with your shell's privileges, before the UI appears.
:::

## Mapping from `crushrc`

| `crushrc` | `crush.json` |
| --- | --- |
| `provider add <id> …` | `providers.<id>` |
| `model add <p>/<id> …` | `providers.<p>.models[]` |
| `model large <p>/<id>` | `models.large` |
| `mcp add <name> …` | `mcp.<name>` |
| `lsp add <name> …` | `lsp.<name>` |
| `hook add <event> …` | `hooks.<Event>[]` |
| `permissions allow …` | `permissions.allowed_tools[]` |
| `permissions deny …` | `options.disabled_tools[]` |
| `option <key> <value>` | `options.<key>` |
| `option skill-path …` | `options.skills_paths[]` |
| `option disable-skill …` | `options.disabled_skills[]` |
| `option context-path …` | `options.context_paths[]` |
| `option global-context-path …` | `options.global_context_paths[]` |
| `option attribution-*` | `options.attribution.*` |
| `option todo-*/dispatch-*` | `options.todo_enforcement.<key>` |
| `option ui <key> <value>` | `options.tui.<key>` |

Note that the JSON names are not a mechanical transliteration of the builtin
names: `permissions deny` writes `options.disabled_tools`, and several
`option` booleans are inverted in JSON (`option auto-summarize false` is
`options.disable_auto_summarize: true`, and the same pattern applies to
`disable_metrics`, `disable_default_providers`, and
`disable_provider_auto_update`). When in doubt, check the schema.

The top-level `options` object also carries a few keys with no `crushrc`
builtin yet: `allowed_commands`, `allow_all_commands` (see
[Permissions](/configuration/permissions#blocked-commands)), and `disable_a2ui`
(see [A2UI](/features/a2ui)). Top-level `tools` tunes the
`glob`, `grep`, and `ls` tool limits.

:::info[Fork feature]
These `crush.json` keys do not exist upstream: `options.allowed_commands`,
`options.allow_all_commands`, `options.disable_a2ui`,
`options.todo_enforcement` ([multi-agent](/agents/todo-enforcement)), the
top-level `agents` block ([multi-agent](/agents/configuration)), the
top-level `embeddings` block ([semantic search](/features/semantic-search)),
and the per-server `channel_enabled` ([channels](/features/channels)). See
[What this fork adds](/fork#configuration).
:::

## Top-level `env`

The top-level `env` field sets environment variables at startup, **before**
providers are configured. This is the way to set variables that affect provider
authentication — the AWS SDK credential chain, for instance — without wrapping
`crush` in a shell script:

```json
{
  "$schema": "https://charm.land/crush.json",
  "env": {
    "AWS_PROFILE": "my-sso-profile"
  }
}
```

Values support the same `$VAR` and `$(command)` expansion as other config
fields.

## The `agents` block

The top-level `agents` object customizes the built-in agents and defines new
ones ([multi-agent](/agents/configuration)). Every field is optional: a null or
omitted field inherits from the built-in with the same id, lists replace
rather than append, and `0` or `"off"` disables the knob it configures.

The built-ins are `coder`, `plan`, and `task`, plus `worker`, the dispatch
agent definition the multi-agent runtime uses. Their roles (`main`,
`subagent`, `dispatch`) cannot be changed, and `coder` cannot be disabled.

```json
{
  "$schema": "https://charm.land/crush.json",
  "agents": {
    "$defaults": { "todos": { "hard_gate": true } },
    "coder": {
      "description": "Ship it.",
      "tools": { "deny": ["sourcegraph"] },
      "mcp": { "allow": ["github"] }
    },
    "worker": {
      "model": { "provider": "deepseek", "model": "deepseek-chat" },
      "kill": { "after_ignored_nudges": 3, "stall": "5m", "timeout": "30m" }
    }
  }
}
```

Field guide:

| Field | Values |
| --- | --- |
| `role` | `main`, `subagent`, or `dispatch`. A new agent must be `dispatch`. |
| `runtime` | `builtin` (in process) or `a2a` (an external [Agent Card](https://a2a-protocol.org)). |
| `model` | `"large"`, `"small"`, or `{ "provider": "...", "model": "..." }`. |
| `tools` | `allow` and `deny` lists; entries are tool names, `@read`, `@write`, or `*`. Effective tools are allow minus deny minus `options.disabled_tools`, so a definition can narrow but never widen your tool policy. |
| `mcp` | `allow` list; entries are `*`, a server id from `mcp`, or `server:tool`. |
| `todos` | `nudge`, `nudge_after_tool_calls`, `hard_gate` (see [Todo enforcement](/agents/todo-enforcement)). |
| `kill` | `after_ignored_nudges`, `stall`, `timeout`. Dispatch agents only. |
| `context_paths` | Context files for the agent, overriding `options.context_paths`. |
| `$defaults` | Only `todos` and `kill`: the knobs every agent inherits. |

An `a2a` agent is defined by its external card, so it may only set `role`,
`name`, `description`, `disabled`, `card`, `auth`, `workspace` (must be
`none`), `transport`, and `kill` (only `timeout`).

Fields the runtime parses but does not act on yet (`model` pins, `prompt`,
`skills`, `workspace`, and the `a2a` machinery) load with a one-time warning.
They are carried for the multi-agent epic: #432 wires agents to these
definitions, #433 adds the agent parameter, and #434 fetches external cards.
The `agent` builtin defines the same block from `crushrc`
([#431](https://github.com/joestump-agent/crush/issues/431)).

## Full example

```jsonc
{
  "$schema": "https://charm.land/crush.json",

  "env": {
    "AWS_PROFILE": "my-sso-profile"
  },

  "providers": {
    "deepseek": {
      "type": "openai-compat",
      "base_url": "https://api.deepseek.com/v1",
      "api_key": "$DEEPSEEK_API_KEY",
      "models": [
        {
          "id": "deepseek-chat",
          "name": "Deepseek V3",
          "context_window": 64000,
          "default_max_tokens": 5000
        }
      ]
    }
  },

  "models": {
    "large": { "provider": "deepseek", "model": "deepseek-chat" }
  },

  "lsp": {
    "go": { "command": "gopls" }
  },

  "mcp": {
    "github": {
      "type": "http",
      "url": "https://api.githubcopilot.com/mcp/",
      "headers": { "Authorization": "Bearer $GH_PAT" }
    }
  },

  "hooks": {
    "PreToolUse": [
      { "matcher": "^bash$", "command": "./hooks/no-rm-rf.sh" }
    ]
  },

  "permissions": {
    "allowed_tools": ["view", "ls", "grep"]
  },

  "options": {
    "allowed_commands": ["ssh", "curl"],
    "tui": { "diff": "unified" }
  }
}
```
