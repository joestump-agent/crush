---
id: tools
title: Tool reference
sidebar_position: 2
description: Every tool the agent can call, what it does, and how to allow or deny it.
---

# Tool reference

These are the names you use with
[`permissions allow` / `permissions deny`](/configuration/permissions), and the
names [hook matchers](/features/hooks) test against.

## Files

| Tool | Does |
| --- | --- |
| `view` | Read a file by path with line numbers; supports offset and line limit |
| `edit` | Exact find-and-replace in one file; can also create or delete content |
| `multiedit` | Several find-and-replace edits to one file in a single operation, applied sequentially. Preferred over repeated `edit` on the same file |
| `write` | Create or overwrite a file, auto-creating parent directories. Cannot append |

## Search

| Tool | Does |
| --- | --- |
| `glob` | Find files by name pattern, sorted by modification time |
| `grep` | Search file contents by regex or literal, sorted by modification time |
| `ls` | List files and directories as a tree, skipping hidden and system dirs |
| `sourcegraph` | Search code across public GitHub repos via Sourcegraph — regex, language, repo, and file filters |
| `semantic_search` | Vector search over the local index by meaning. Registered only when an embeddings provider is configured — see [Semantic search](/features/semantic-search) |
| `semantic_index` | Build or refresh that index. Incremental; unchanged files are skipped |

The `glob`, `grep`, and `ls` result limits are tunable under the top-level
`tools` key in [`crush.json`](/configuration/json).

:::info[Fork feature]
`semantic_search` and `semantic_index` are additions in the
`joestump-agent/crush` fork.
:::

## Shell and job control

| Tool | Does |
| --- | --- |
| `bash` | Run shell commands. Long-running commands automatically move to the background and return a shell ID |
| `job_output` | Get stdout/stderr from a background shell by ID; `wait=true` blocks until it finishes |
| `job_kill` | Terminate a background shell |

`bash` runs through Crush's embedded POSIX shell
([`mvdan.cc/sh`](https://mvdan.cc/sh)), so it behaves identically on every
platform — Windows included. [`jq` is a built-in](/features/skills#jq); no
external binary needed.

A [blocklist](/configuration/permissions#blocked-commands) sits in front of the
`bash` tool's permission flow. The fork adds
[`allowed_commands`](/configuration/permissions#blocked-commands) to punch
named holes in it.

:::note[Upstream, not fork]
Background jobs — `bash` backgrounding long-running commands, plus `job_output`
and `job_kill` — are upstream Crush, not a fork addition. Bugs in them belong
[upstream](https://github.com/charmbracelet/crush/issues).
:::

## Network

| Tool | Does |
| --- | --- |
| `fetch` | Fetch raw content from a URL as text, markdown, or HTML. No AI processing |
| `download` | Download a URL straight to a local file — binary-safe and streaming |
| `agentic_fetch` | Fetch or search using an AI sub-agent that can extract, summarise, and answer questions. Slower and costlier than `fetch` |

Two more exist for **sub-agents only** and are not on the top-level agent's
list: `web_search` (DuckDuckGo — titles, URLs, snippets) and `web_fetch` (a URL
as markdown, with pages over 50KB saved to a temp file for `grep`/`view`).

## Language servers

Registered when you have configured at least one LSP, or `auto-lsp` is on
(the default). See [Language servers](/features/lsp).

| Tool | Does |
| --- | --- |
| `lsp_diagnostics` | Errors, warnings, and hints for a file or the project |
| `lsp_definition` | Find where a symbol is defined |
| `lsp_references` | Find every reference to a symbol |
| `lsp_symbols` | Structured outline of a file |
| `lsp_call_hierarchy` | Incoming or outgoing calls for a symbol |
| `lsp_rename` | True semantic rename across all files |
| `lsp_replace_symbol` | Replace, insert, or delete a whole symbol by name |
| `lsp_restart` | Restart one or all LSP clients |

## MCP

Registered when at least one MCP server is configured. See
[MCP](/features/mcp#prompts-and-resources).

| Tool | Does |
| --- | --- |
| `list_mcp_resources` | List resource URIs and templates from a server |
| `read_mcp_resource` | Read a resource by URI |
| `list_mcp_prompts` | List a server's prompts |
| `call_mcp_prompt` | Invoke a prompt and get its rendered content |

Tools exposed *by* MCP servers arrive as `mcp_<server>_<tool>`.

:::info[Fork feature]
`list_mcp_prompts` and `call_mcp_prompt` are additions in the
`joestump-agent/crush` fork. See
[MCP prompts](/features/mcp#prompts-and-resources).
:::

## Scheduling

:::info[Fork feature]
See [Scheduled tasks](/features/scheduled-tasks).
:::

| Tool | Does |
| --- | --- |
| `CronCreate` | Schedule a prompt on a cron expression |
| `CronList` | List the session's scheduled tasks |
| `CronDelete` | Cancel a task by ID |

## Session and introspection

| Tool | Does |
| --- | --- |
| `todos` | A structured task list for multi-step work; each task is pending, in progress, or completed |
| `question` | Ask you a structured question and wait for the answer. Interactive sessions only; never available to sub-agents |
| `crush_info` | Crush's live runtime state: active model and provider, LSP/MCP status, skills, hooks, permissions, disabled tools |
| `crush_logs` | Read Crush's internal application logs — useful when debugging Crush itself |
| `agent` | Launch a sub-agent with `glob`, `grep`, `ls`, and `view`, for searches that need several tries |

## What sub-agents get

Sub-agents (`agent`, `agentic_fetch`) run with a restricted tool set and are
**not** intercepted by `PreToolUse` hooks, so a single delegated turn doesn't
fire your hooks N times. The outer sub-agent tool call itself *is* hooked.

## Checking what's live

Ask the agent — `crush_info` reports the tools actually registered in the
current session, along with which are allowed, denied, and disabled.
