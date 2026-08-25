---
id: sessions
title: Sessions and projects
sidebar_position: 10
description: Multiple work contexts per project, resuming them, and managing them from the CLI.
---

# Sessions and projects

Crush is session-based: each conversation is a session with its own history,
title, cost, and token accounting, scoped to the project directory it was
started in.

## In the TUI

| Keys | Does |
| --- | --- |
| <kbd>ctrl+n</kbd> | Start a new session |
| <kbd>ctrl+s</kbd> | Open the session picker |

Sessions are titled automatically by the **small** model as soon as there is
enough to title.

## Resuming from the CLI

```bash
# Continue the most recent session.
crush --continue
crush -C

# Continue a specific session.
crush --session <id>
crush -s <id>
```

## Managing sessions

```bash
crush session list            # or: crush sessions ls
crush session show <id>
crush session last
crush session rename <id> "a better title"
crush session delete <id>     # or: crush session rm <id>
```

An `<id>` may be a UUID, a full hash, or a hash prefix.

Every subcommand takes `--json` for machine-readable output, which makes the
whole thing scriptable — and is why the help text mentions agents explicitly.

```bash
crush session list --json | jq -r '.[] | "\(.id)  \(.title)"'
```

## Projects

Crush keeps a registry of directories it has been run in:

```bash
crush projects
```

Sessions are grouped by working directory. Two clients that resolve to the same
`--cwd` share a
[workspace](/features/server-and-workspaces) — the same session list, message
history, permission queue, LSP, and MCP state.

## Usage statistics

```bash
crush stats                          # this project
crush stats --all                    # every known project
crush stats --crawl-dir ~/src        # crawl a tree for Crush projects
```

Reports token usage, cost, activity by day, by hour, by day of week, by model,
and average response time.

## Summarising long conversations

Crush summarises a conversation automatically when it grows past what the
context window comfortably holds, preserving the thread while dropping the bulk.

Turn it off if you would rather manage context yourself:

```bash
option auto-summarize false
```

## Where session data lives

Sessions live in the project's Crush data directory as SQLite. Confirm the
paths on your machine:

```bash
crush dirs
```

Change it with `option data-directory`, or per-launch with `--data-dir` / `-D`.
