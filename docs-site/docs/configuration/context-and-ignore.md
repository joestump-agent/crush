---
id: context-and-ignore
title: Context and ignore files
sidebar_position: 6
description: AGENTS.md, CRUSH.md, global context paths, .crushignore, and project initialization.
---

# Context and ignore files

## Project context

When you initialize a project, Crush analyses your codebase and writes a context
file with the build commands, code patterns, and conventions it discovered. That
file is loaded into every future session in the project.

Run `/init` from the command palette, or let Crush prompt you on first launch.

The default filename is `AGENTS.md`. Change it:

```bash
# crushrc
option initialize-as CRUSH.md
```

Useful if you prefer a different convention or want the file somewhere specific
(`docs/LLMs.md`, say).

Add more project context paths:

```bash
option context-path ./docs/architecture.md
option context-path ./docs/conventions/
```

## Global context

Crush automatically includes two files for cross-project instructions. Think of
them as personal additions to the system prompt.

| File | For |
| --- | --- |
| `~/.config/crush/CRUSH.md` | Crush-specific rules that would confuse other agentic coding tools. If Crush is the only agent you use, this is the only one you need. |
| `~/.config/AGENTS.md` | Generic instructions other coding tools might also read. Avoid Crush-specific features here. |

Customise the paths with `global-context-path`. Repeat the command to add more
than one:

```bash
# Load a single markdown file.
option global-context-path "~/path/to/custom/context/file.md"

# Recursively load all Markdown files in a folder.
option global-context-path "/full/path/to/folder/of/files/"
```

To drop paths a `source`-d base config added and start over:

```bash
option reset global-context-path
option global-context-path ~/my/context/
```

## Ignoring files

Crush respects `.gitignore` by default. For things you want in version control
but *not* in Crush's context, add a `.crushignore`:

```gitignore
# .crushignore
vendor/
*.snap
testdata/fixtures/**
docs/generated/
```

Same syntax as `.gitignore`. It can live at the project root or in
subdirectories.

## What Crush is actually loading

```bash
# In-session: ask the agent.
> what context files are loaded?
```

The `crush_info` tool reports Crush's live runtime state — active model and
provider, LSP and MCP status, skills, hooks, permissions, and disabled tools —
so asking is usually faster than reading config. See the
[tool reference](/reference/tools).
