---
id: lsp
title: Language servers
sidebar_position: 1
description: Crush reads your code through LSP — diagnostics, definitions, references, call hierarchies, and semantic renames.
---

# Language servers

Crush uses Language Server Protocol servers for additional context, just like
you do. When an LSP is attached, the agent stops guessing about your code and
starts asking the compiler.

## Configuring

```bash
# crushrc
lsp add go --command "gopls" --env "GOTOOLCHAIN go1.24.5"
lsp add typescript --command "typescript-language-server" --args --stdio
lsp add nix --command "nil"
```

Full flag list in the
[config command reference](/configuration/command-reference#lsp-add):
`--args`, `--env`, `--filetypes`, `--root-markers`, `--timeout`, `--disabled`,
`--init-options`, and `--options`.

```bash
lsp add rust --command rust-analyzer \
  --filetypes rust \
  --root-markers Cargo.toml \
  --options '{"cargo":{"features":"all"}}'
```

## Automatic configuration

Crush configures language servers automatically when it can detect them. Turn
that off if you want only what you declared:

```bash
option auto-lsp false
```

The LSP tools are registered when you have configured at least one LSP **or**
`auto-lsp` is on (which is the default).

## What the agent gets

| Tool | Does |
| --- | --- |
| `lsp_diagnostics` | Errors, warnings, and hints for a file or the whole project |
| `lsp_definition` | Find where a symbol is defined — language-aware, unlike `grep` |
| `lsp_references` | Find every reference to a symbol |
| `lsp_symbols` | Structured outline of a file: functions, types, methods, line ranges |
| `lsp_call_hierarchy` | Incoming calls (who calls this) or outgoing calls (what does this call) |
| `lsp_rename` | True semantic rename across every file |
| `lsp_replace_symbol` | Replace, insert, or delete a whole symbol by name, using LSP boundaries |
| `lsp_restart` | Restart one or all LSP clients when diagnostics go stale |

`lsp_rename` and `lsp_replace_symbol` are the ones that change the character of
the agent's edits: a rename becomes one semantic operation rather than a
multi-file find-and-replace the model has to get right by hand.

## Status and troubleshooting

LSP status shows in the sidebar — `unstarted`, running, or errored. When
diagnostics look wrong or a server has wedged, ask Crush to restart it, or
enable LSP debug logging:

```bash
option debug-lsp true
```

Then read the log:

```bash
crush logs --follow
```

See [Logging](/reference/logging).
