---
id: cli
title: CLI reference
sidebar_position: 1
description: Every crush command and flag.
---

# CLI reference

```text
crush — A terminal-first AI assistant for software development
```

Running `crush` with no subcommand starts the interactive TUI in the current
directory.

## Global flags

These are persistent flags — they work on every subcommand.

| Flag | Short | Does |
| --- | --- | --- |
| `--cwd <dir>` | `-c` | Working directory |
| `--data-dir <dir>` | `-D` | Custom Crush data directory |
| `--debug` | `-d` | Debug logging |
| `--host <url>` | `-H` | Connect to a specific Crush server (advanced) |
| `--channels <server:name>` | | Enable an MCP server as a [channel](/features/channels). Repeatable. |
| `--yolo` | `-y` | Auto-accept all permissions (**dangerous**) |

:::info[Fork feature]
`--yolo` is persistent in this fork, so it works on subcommands too —
`crush --yolo run "…"`. Upstream it is a root-only flag and that invocation
fails with `unknown flag`.
:::

## `crush`

| Flag | Short | Does |
| --- | --- | --- |
| `--session <id>` | `-s` | Continue a previous session by ID |
| `--continue` | `-C` | Continue the most recent session |
| `--allow-commands <cmd>` | | Allow a command from the bash blocklist. Repeatable. Env: `CRUSH_ALLOW_COMMANDS` (comma-separated) |
| `--allow-all-commands` | | Remove **all** bash blocklist restrictions, including package-manager blocks (**dangerous**). Env: `CRUSH_ALLOW_ALL_COMMANDS` |
| `--help` | `-h` | Help |

:::info[Fork feature]
`--allow-commands` and `--allow-all-commands` are fork additions. See
[Permissions](/configuration/permissions#blocked-commands).
:::

```bash
crush
crush -c ~/src/other-project
crush --continue
crush --channels server:webhook
```

## `run`

Alias: `r`. Run a single prompt non-interactively and exit. The prompt comes
from arguments, stdin, or both.

| Flag | Short | Does |
| --- | --- | --- |
| `--quiet` | `-q` | Hide the spinner |
| `--verbose` | `-v` | Show logs |
| `--model <m>` | `-m` | Model to use. Accepts `model` or `provider/model` to disambiguate |
| `--small-model <m>` | | Small model to use |
| `--session <id>` | `-s` | Continue a previous session |
| `--continue` | `-C` | Continue the most recent session |

```bash
crush run "Guess my 5 favorite Pokémon"

# Pipe input from stdin.
curl https://charm.land | crush run "Summarize this website"

# Read from a file.
crush run "What is this code doing?" <<< prrr.go

# Redirect output to a file.
crush run "Generate a hot README for this project" > MY_HOT_README.md

# Continue a session.
crush run --continue "Follow up on your last response"
```

## `session`

Aliases: `sessions`, `s`. Every subcommand takes `--json` for machine-readable
output. An `<id>` may be a UUID, a full hash, or a hash prefix.

```text
crush session list            # alias: ls
crush session show <id>
crush session last
crush session delete <id>     # alias: rm
crush session rename <id> <title>
```

## `models`

List every model available from the configured providers.

```bash
crush models
```

## `projects`

List directories Crush has been run in.

| Flag | Does |
| --- | --- |
| `--json` | Output as JSON |

## `logs`

View the project's Crush logs.

| Flag | Short | Does |
| --- | --- | --- |
| `--follow` | `-f` | Follow log output |
| `--tail <n>` | `-t` | Show only the last N lines (default 1000) |

```bash
crush logs
crush logs --tail 500
crush logs --follow
```

## `login`

Alias: `auth`. Authenticate Crush with a platform. Available platforms:
`hyper` (default), `copilot`.

| Flag | Short | Does |
| --- | --- | --- |
| `--force` | `-f` | Re-authenticate even if already logged in |

```bash
crush login              # Charm Hyper
crush login copilot
crush login -f copilot
```

## `logout`

| Flag | Short | Does |
| --- | --- | --- |
| `--force` | `-f` | Skip the confirmation prompt |

## `update-providers`

Update the provider and model catalog.

| Flag | Does |
| --- | --- |
| `--source <s>` | Provider source: `catwalk` (default) or `hyper` |

```bash
crush update-providers                      # from Catwalk
crush update-providers https://example.com/ # from a custom base URL
crush update-providers ./providers.json     # from a local file
crush update-providers embedded             # reset to the build's embedded copy
```

## `server`

Start the Crush server. See
[Server and workspaces](/features/server-and-workspaces).

| Flag | Short | Does |
| --- | --- | --- |
| `--host <url>` | `-H` | TCP or Unix socket to listen on |

```bash
crush server
crush server --host tcp://127.0.0.1:8099
crush server --host unix:///tmp/crush.sock
```

## `stats`

Show usage statistics — tokens, cost, and activity patterns.

| Flag | Does |
| --- | --- |
| `--all` | Aggregate stats from every known project |
| `--crawl-dir <dir>` | Crawl a directory recursively for Crush projects and aggregate |

## `dirs`

Print the resolved config and data directories. The first thing to run when
config isn't being picked up.

## `schema`

Generate the JSON schema for `crush.json` from the running build's config types.

```bash
crush schema > schema.json
```
