---
id: troubleshooting
title: Troubleshooting
sidebar_position: 6
description: The things that go wrong most often, and what to check first.
---

# Troubleshooting

## Copy and paste isn't working

Unix-like systems need an external clipboard helper:

| Environment | Tool |
| --- | --- |
| macOS | Native support |
| Windows | Native support |
| Linux/BSD + Wayland | `wl-copy` and `wl-paste` |
| Linux/BSD + X11 | `xclip` or `xsel` |

## My config isn't being picked up

```bash
crush dirs
```

That prints the config and data directories Crush actually resolved. Common
causes:

- A `crushrc` in a **data** directory. Crush deliberately does not execute one
  from `~/.local/share/crush` or `%LOCALAPPDATA%\crush` — those hold state.
- `XDG_CONFIG_HOME` pointing somewhere unexpected.
- Both a `crushrc` and a `crush.json` in the same folder. They merge, `crushrc`
  wins, and Crush logs a warning — check [the logs](/reference/logging).
- A project-level config overriding your global one. Project beats global by
  design.

## A hook seems to do nothing

- A hook that exits with anything other than `2` or `49` is treated as a
  **non-blocking error**: it is logged and ignored, and the tool call proceeds.
  Check [the logs](/reference/logging).
- `command` is resolved relative to your **working directory**, not the config
  file. A relative path in a global config will not resolve — use an absolute
  path.
- Shebang'd scripts need the interpreter on `PATH`. Crush falls back to a `PATH`
  lookup of the base name and logs it at debug level.
- Sub-agent tool calls are not hooked by design. See
  [scope](/features/hooks#pretooluse).

## An MCP server won't start

- MCP init is bounded, so a wedged server cannot blank the app. Failures are
  reported in the sidebar with their error.
- Check that the `command` exists and that `args` are separate entries, not one
  space-joined string.
- For HTTP servers, an `Authorization` header whose value resolves to an empty
  string is **dropped** — if your token env var is unset, the request goes out
  unauthenticated. Check with `[ -n "$GH_PAT" ] && echo set || echo empty`.
- Raise `--timeout` for servers that are slow to boot.

## A channel server is configured but silent

Listing a server under `mcp` is not enough — channels are gated behind an
explicit opt-in. You need **both**:

1. `--channels server:<name>` or `"channel_enabled": true`, and
2. the server declaring the `claude/channel` capability at `initialize`.

Servers that are live channels are marked `channel` in the MCP list, which is
the quickest way to confirm the opt-in took. See
[Channels](/features/channels#opting-in).

## The model can't see a tool

Three separate things hide a tool:

| Cause | Fix |
| --- | --- |
| `permissions deny <tool>` | Remove it — a denied tool is invisible to the model |
| Per-server `--disabled-tools` / `--enabled-tools` | Adjust the MCP server config |
| The tool is conditionally registered | LSP tools need an LSP or `auto-lsp`; MCP resource/prompt tools need a configured server; `question` is interactive-only; `sidekick_update` needs a Sidekick and A2UI enabled |

`crush_info` reports what is actually registered right now — ask the agent.

## A bash command is blocked

The `bash` tool blocks a set of dangerous commands by default. Two distinct
knobs:

- `allowed_commands` / `--allow-commands` removes commands from the
  **exact-command** blocklist. It does *not* unlock package-manager argument
  blocks like `apt install`.
- `allow_all_commands` / `--allow-all-commands` removes everything, including
  those.

And note that allowing a command does **not** auto-approve it — you still get
the permission prompt unless you are in yolo mode. See
[Permissions](/configuration/permissions#blocked-commands).

## A skill isn't showing up

- The `name` must be lowercase alphanumeric with single hyphens.
- `disable-model-invocation: true` hides it from the model on purpose; it is
  still user-invocable.
- `option disable-skill <name>` hides it entirely.
- The `/skills` dialog lists every discovered skill and reports parse failures
  with the error and its source, rather than dropping them silently.

## LSP diagnostics are stale

Ask the agent to restart the server (`lsp_restart`), and turn on
`option debug-lsp true` if it keeps happening.

## Two clients disagree about yolo mode

`--yolo` and `--debug` are **first-wins** in a shared workspace. The first
client to create a workspace fixes them; a later client at the same `--cwd`
does not change them. A debug log line records the mismatch. See
[first-wins flags](/features/server-and-workspaces#first-wins-flags).

## Reporting a bug

- Upstream Crush behaviour →
  [charmbracelet/crush/issues](https://github.com/charmbracelet/crush/issues)
- A [fork addition](/introduction#about-this-fork) →
  [joestump-agent/crush/issues](https://github.com/joestump-agent/crush/issues)
- A missing or stale provider/model →
  [Catwalk](https://github.com/charmbracelet/catwalk)

Include the output of `crush logs --tail 200` and, if config is involved,
`crush dirs`.
