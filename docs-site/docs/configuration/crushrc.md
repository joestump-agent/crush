---
id: crushrc
title: Configuring Crush
sidebar_label: crushrc (Bash)
sidebar_position: 1
description: crushrc is just Bash with Crush builtins — where it lives, how it merges, and what it can do.
---

# Configuring Crush

Crush runs great with no configuration at all. When you do want to customise it,
you write a **`crushrc`**.

A `crushrc` is just Bash with some Crush-specific builtins. It is a lot like a
`.bashrc`, only for your Crush. Because Crush has a native, built-in Bash
interpreter, Bash-based config works identically on every platform — Windows
included, with no WSL, Git Bash, or Cygwin.

:::tip[Ask Crush to do it]
Crush ships a built-in `crush-config` skill. Most of the time you can just
describe what you want and Crush will write the config for you.
:::

## A worked example

```bash
# Add Ollama.
provider add ollama --type ollama --base-url "http://localhost:11434/v1"

# Register a model on Ollama.
model add ollama/llama3.3 --name "Llama 3.3" --context-window 128000

# Auto-approve some tools.
permissions allow view edit

# Include some other file on a specific machine.
if [[ $HOSTNAME == "babysquid" ]]; then
    source ~/my-stuff/babysquid.sh
fi

# Add an MCP server, with a GitHub API token stored in 1Password.
mcp add github \
  --type http \
  --url "https://api.githubcopilot.com/mcp/" \
  --header Authorization "Bearer $(op read 'op://my-secret-key')"
```

## Why Bash?

Because it is a real shell, you get for free the things a static config format
makes you invent:

- **Conditionals** — different config per host, OS, or repo.
- **Includes** — `source` a shared team base config.
- **Secrets** — `$(op read …)`, `$(pass show …)`, `$(gcloud auth print-access-token)`.
- **Variables and loops** — register ten models without ten copy-pasted blocks.
- **Comments that are actually comments** — no JSON-with-comments dialect.

## Config versioning

Not breaking the config API matters, but when you do need to branch on the
running version, `$CRUSH_VERSION` is available inside a `crushrc`:

```bash
if [[ $CRUSH_VERSION == "0.85.*" ]]; then
    option debug true
fi
```

## Where config lives

Everything found is merged. Lower priority numbers win.

| Priority | Unix-like | Windows |
| --- | --- | --- |
| 1 | `./.crushrc` | `.\.crushrc` |
| 2 | `./crushrc` | `.\crushrc` |
| 3 | `$XDG_CONFIG_HOME/crush/crushrc`<br />(`~/.config/crush/crushrc`) | `%USERPROFILE%\.config\crush\crushrc` |

Legacy [`crush.json`](/configuration/json) uses `.crush.json` / `crush.json` in
the same directories. Project settings override global ones, and a `crushrc`
overrides JSON in the same directory. If a folder has both, they merge and Crush
logs a warning.

Crush respects the
[XDG Base Directory Specification](https://specifications.freedesktop.org/basedir-spec/basedir-spec-latest.html),
so your paths depend on `XDG_CONFIG_HOME`.

Override the config and data locations entirely:

```bash
export CRUSH_GLOBAL_CONFIG=/path/to/config
export CRUSH_GLOBAL_DATA=/path/to/data
```

Confirm what Crush actually resolved:

```bash
crush dirs
```

### State is not config

Crush also stores ephemeral application state as JSON:

```bash
# Unix
$HOME/.local/share/crush/crush.json

# Windows
%LOCALAPPDATA%\crush\crush.json
```

This is machine-owned state, not configuration. Don't edit it by hand. Crush
does **not** discover or execute a `crushrc` from a data directory.

## Composing configs

Because it's Bash, a shared base config is just a `source`:

```bash
# ~/.config/crush/crushrc
source ~/team/crush-base.sh    # providers, a few skill paths

# …but on this machine, drop a skill path the base added and add my own.
option reset skill-path
option skill-path ~/my/skills
```

`remove`, `rm`, and `option reset` all act on whatever was set earlier in the
script or pulled in via `source`. Later lines win, exactly like a shell.

## The builtins

Entity commands use `add` to create or update and `remove` (aliased `rm`) to
delete. Booleans accept `true/false/1/0/yes/no` in any case.

| Builtin | Manages |
| --- | --- |
| `provider` | [Model providers](/configuration/providers) |
| `model` | Custom models and the large/small slots |
| `mcp` | [MCP servers](/features/mcp) |
| `lsp` | [Language servers](/features/lsp) |
| `hook` | [Hooks](/features/hooks) |
| `permissions` | [Tool permissions](/configuration/permissions) |
| `option` | General behaviour, paths, attribution, and the TUI |

Every flag for every one of them is in the
**[config command reference](/configuration/command-reference)**.

## Security

Both `crushrc` and `crush.json` are **trusted code**. `crushrc` runs in a full
shell, and any `$(...)` in `crush.json` runs at load time — before the UI
appears, with your privileges.

- Don't launch Crush in a directory whose config you haven't reviewed.
- Don't `source` files from the internet into your config.
- Prefer reading secrets from a password manager over pasting them in.

## What about JSON?

`crush.json` is still fully supported but should be considered deprecated. New
configuration options are only added to the Bash format. See
[Legacy JSON config](/configuration/json).
