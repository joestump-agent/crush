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
| `option ui <key> <value>` | `options.tui.<key>` |

Note that the JSON names are not a mechanical transliteration of the builtin
names: `permissions deny` writes `options.disabled_tools`, and several
`option` booleans are inverted in JSON (`option auto-summarize false` is
`options.disable_auto_summarize: true`, and the same pattern applies to
`disable_metrics`, `disable_default_providers`, and
`disable_provider_auto_update`). When in doubt, check the schema.

The top-level `options` object also carries a few keys with no `crushrc`
builtin yet: `allowed_commands`, `allow_all_commands` (see
[Permissions](/configuration/permissions#blocked-commands)) and `disable_a2ui`
(see [A2UI](/features/a2ui)). Top-level `tools` tunes the `glob`, `grep`, and
`ls` tool limits.

:::info[Fork feature]
These `crush.json` keys do not exist upstream: `options.allowed_commands`,
`options.allow_all_commands`, `options.disable_a2ui`, the top-level
`embeddings` block ([semantic search](/features/semantic-search)), and the
per-server `channel_enabled` ([channels](/features/channels)). See
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
