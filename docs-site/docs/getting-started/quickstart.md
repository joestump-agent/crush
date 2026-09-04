---
id: quickstart
title: Quickstart
sidebar_position: 2
description: First run, picking a model, and the keys and shortcuts you need on day one.
---

# Quickstart

## 1. Start Crush in your project

```bash
cd ~/src/my-project
crush
```

Crush is project-scoped: it reads config, context files, and skills relative to
the working directory. Use `--cwd` (or `-c`) to point it somewhere else.

## 2. Pick a model

Press <kbd>ctrl+l</kbd> to open the model picker.

The quickest path is [Hyper](https://hyper.charm.land), Charm's official Crush
provider — subscription-based with a free tier, optimised for Crush, privacy
focused with zero data retention, and designed to comply with GDPR. Choose it
in the picker and follow the auth steps.

Otherwise pick any provider in the list and paste an API key, or set the
provider's environment variable before launching. See
[Providers and API keys](/getting-started/providers-and-api-keys) for the
complete table.

Crush uses two model slots:

- **large** — the coding model, used for the main agent.
- **small** — a cheaper model used for titles and summaries.

## 3. Let Crush learn the project

Run `/init` in the session (or `crush` will suggest it). Crush analyses the
codebase and writes an `AGENTS.md` context file with build commands, code
patterns, and conventions it found. That file is loaded into every subsequent
session in this project.

Rename it with `option initialize-as CRUSH.md` if you prefer a different
convention. See [Context and ignore files](/configuration/context-and-ignore).

## 4. Work

Type a prompt and hit <kbd>enter</kbd>. Crush asks for permission before each
tool call that touches your machine. To stop being asked for the safe ones:

```bash
# In your crushrc.
permissions allow view ls grep
```

See [Permissions](/configuration/permissions) — including `--yolo`, which skips
every prompt and should be used with care.

## The shortcuts that matter on day one

| Keys | Does |
| --- | --- |
| <kbd>ctrl+p</kbd> or <kbd>/</kbd> | Command palette |
| <kbd>ctrl+l</kbd> | Model picker |
| <kbd>ctrl+s</kbd> | Session picker |
| <kbd>ctrl+n</kbd> | New session |
| <kbd>@</kbd> | Mention a file |
| <kbd>ctrl+f</kbd> | Attach a file |
| <kbd>ctrl+o</kbd> | Open your `$EDITOR` for the prompt |
| <kbd>ctrl+y</kbd> | Toggle yolo mode |
| <kbd>esc</kbd> | Cancel the running turn |
| <kbd>ctrl+g</kbd> | Show more help |

The full list is in the [keybindings reference](/reference/keybindings).

## Non-interactive runs

Crush also runs one-shot prompts, which makes it scriptable and CI-friendly:

```bash
crush run "summarise the changes on this branch"
git diff | crush run "review this diff"
```

See the [CLI reference](/reference/cli#run).

## Next

- **[Configuring Crush](/configuration/crushrc)** — `crushrc`, the Bash config
  format.
- **[MCP](/features/mcp)** — plug in external tools.
- **[Skills](/features/skills)** — reusable instruction packages, four of which
  ship built in.
