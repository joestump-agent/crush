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
| `todos` | A structured task list for multi-step work; each task is pending, in progress, or completed. In the fork, an agent that works without one is nudged to start one — see [Todo enforcement](/agents/todo-enforcement) |
| `question` | Ask you a structured question and wait for the answer. Interactive sessions only; never available to sub-agents. A [dispatched agent](#what-dispatched-agents-get) gets it while the session is interactive |
| `crush_info` | Crush's live runtime state: active model and provider, LSP/MCP status, skills, hooks, permissions, disabled tools |
| `crush_logs` | Read Crush's internal application logs — useful when debugging Crush itself |
| `agent` | Launch a sub-agent with `glob`, `grep`, `ls`, and `view`, for searches that need several tries |

## Dispatched agents

:::info[Fork feature]
See [Multi-agent dispatch](/agents/overview).
:::

| Tool | Does |
| --- | --- |
| `dispatch_agent` | Start an independent agent in its own git worktree, on a fresh `crush-dispatch-*` branch cut from a committed revision. It runs in the background and the tool returns a running handle at once; the result — findings plus a diff summary — arrives as a follow-up turn when it finishes. Parameters: `prompt` (required), `model` (`large` or `small`; default `small`), `skills` (default: every discovered skill), `branch` (base revision; default the current branch), `handle`, and `role` |
| `message_agent` | Steer a running dispatched agent: the message lands as its next input, and the reply appears on the agent's block in the chat, not as the tool's result. Address it by `session_id` or `handle`, with the text in `message`. A finished agent refuses — dispatch a new one |

Both are main-agent tools: neither sub-agents nor dispatched agents get them,
so delegation is one level deep. Both are on by default; deny them to turn
dispatch off:

```bash
permissions deny dispatch_agent message_agent
```

`dispatch_agent` needs a git repository — anywhere else it returns
`dispatch unavailable`. The `message_agent` schema currently marks
`session_id` as required even when the model addresses the agent by handle.
Tracked as [#400](https://github.com/joestump-agent/crush/issues/400).

## What sub-agents get

Sub-agents (`agent`, `agentic_fetch`) run with a restricted tool set and are
**not** intercepted by `PreToolUse` hooks, so a single delegated turn doesn't
fire your hooks N times. The outer sub-agent tool call itself *is* hooked.

## What dispatched agents get

A dispatched agent gets the `agent` sub-agent's read-only set — `glob`,
`grep`, `ls`, `view`, and, when language servers are on, `lsp_definition`,
`lsp_symbols`, and `lsp_call_hierarchy` — plus `bash`, `edit`, `multiedit`,
`write`, and `todos`. Every path-based tool is rooted at the agent's worktree.

While the session is interactive it also gets `question`: each question
pauses the dispatched run and appears in your question prompt labeled with the
agent's `@handle`, and the run resumes with your answer.

It never gets `agent`, `agentic_fetch`, MCP tools, semantic search,
`dispatch_agent`, or `message_agent`. The outer `dispatch_agent` and
`message_agent` calls are hooked like any top-level tool call.

:::warning[Known issue]
- Dispatched agents are **not** intercepted by `PreToolUse` hooks, although
  unlike sub-agents they can run `bash` and write files. A hook that blocks
  `git push -f` does not stop one. Tracked as [#377](https://github.com/joestump-agent/crush/issues/377).
- `permissions deny` and `options.disabled_tools` can narrow the read-only
  tools, but a dispatched agent always gets `bash`, `edit`, `multiedit`,
  `write`, and `todos` back. Tracked as [#376](https://github.com/joestump-agent/crush/issues/376).
- `job_output` and `job_kill` are missing, so a long-running command that
  `bash` moves to the background cannot be read back. Tracked as [#384](https://github.com/joestump-agent/crush/issues/384).
:::

## Checking what's live

Ask the agent — `crush_info` reports the tools actually registered in the
current session, along with which are allowed, denied, and disabled.
