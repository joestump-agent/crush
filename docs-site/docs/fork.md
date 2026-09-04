---
id: fork
title: What this fork adds
sidebar_label: What this fork adds
sidebar_position: 2
description: Every feature, improvement, and fix that joestump-agent/crush carries on top of charmbracelet/crush — and the things it does not.
---

# What this fork adds

These docs cover
**[`joestump-agent/crush`](https://github.com/joestump-agent/crush)**, a fork
that tracks [`charmbracelet/crush`](https://github.com/charmbracelet/crush)
upstream and adds features on top. Everything upstream does, this fork does.
This page is the full inventory of what it adds beyond that.

:::info[Fork feature]
Throughout these docs, a callout shaped like this one marks behaviour that only
exists in the fork. If a page has no such callout, what it describes is
upstream Crush and belongs
[upstream](https://github.com/charmbracelet/crush/issues) when it breaks.
:::

## Major features

New subsystems — each one has its own page.

| Feature | What it is |
| --- | --- |
| **[A2UI rendering](/features/a2ui)** | The model can emit [A2UI](https://a2ui.org) documents and Crush draws them inline — cards, lists, buttons, forms, dashboards — via [a2tea](https://github.com/joestump-agent/a2tea). Interactive surfaces round-trip: a button press goes back to the model. Includes MCP resource surfaces and an `a2ui` capability advertised at MCP `initialize`. |
| **[Scheduled tasks](/features/scheduled-tasks)** | `CronCreate`, `CronList`, and `CronDelete` tools that re-run a prompt on a cron schedule, with a scheduler, one-shot support, and a pills-panel entry showing what is queued. |
| **[Semantic search](/features/semantic-search)** | A local vector index over the repo — chunked by symbol, embedded through any OpenAI-compatible endpoint, stored in the project's own SQLite via `sqlite-vec`. Adds the `semantic_search` and `semantic_index` tools and a symbol-extraction package. |
| **[Channel reply routing](/features/channels)** | The channel *push* mechanism is upstream. The fork adds the `channel_enabled` config key, a `channel_reply` tool, server-side routing that delivers each event exactly once per workspace, auto-creation of a session for an inbound event, and replies that go back only to the channel they came from. |
| **[MCP prompts](/features/mcp#prompts-and-resources)** | `list_mcp_prompts` and `call_mcp_prompt` tools, prompts offered in the inline `/` completions and the command palette, and prompts loaded at startup rather than on first use. |

## Quality-of-life improvements

Smaller things you notice in the terminal rather than in a config file.

### Chat

| Improvement | Detail |
| --- | --- |
| Click-to-copy | Finished assistant *and* user messages carry a right-aligned `⎘` icon that copies the message. Shown on focus, pinned to the item's right edge. |
| Clickable hyperlinks | A plain click on a link in the transcript opens it in your browser. |
| Channel message styling | Channel-originated messages render with a `sender / via / at` metadata line below the body, so you can tell a webhook from a human. |

### Composer

| Improvement | Detail |
| --- | --- |
| Syntax-highlighted composer | A vendored textarea with a highlighter, so `@file`, `/skill`, and MCP-prompt tokens are visibly distinct as you type. |
| Atomic token editing | Backspace over a completed token deletes the whole token — and drops its attachment, if it had one — instead of eating one character at a time. |
| Text-file attachments | Attach JSON, YAML, Markdown, and source files, not just images. One shared text predicate everywhere, with binary sniffing to reject the rest. |
| Attachment chips | A clickable remove button on each chip, correct hit zones, and chips that survive sending. |
| `/skill` completions | Skills complete inline from `/`, alongside commands and MCP prompts. |

### Dialogs and palette

| Improvement | Detail |
| --- | --- |
| **Skills dialog** | Palette entry **Skills** — list every skill, see load errors, reload from disk, toggle individual skills off. |
| **MCP servers dialog** | Palette entry **MCP Servers** — server status, reconnect, refresh, and a marker on servers acting as channels. |
| **Channels dialog** | Palette entry **Channels** — see and manage active channels. |
| Hierarchical sub-menus | The command palette nests related commands instead of one flat list, with `back`/`backspace` navigation. |
| Model discovery reload | Re-run provider/model discovery from the `/models` dialog without restarting. |

### Sidebar and header

| Improvement | Detail |
| --- | --- |
| Focusable sidebar | Three-state <kbd>tab</kbd> focus, mouse scrolling, a scrollbar instead of a focus border, and irrelevant keybindings hidden while it is focused. |
| Channels column | Active channels shown on the landing page and in the sidebar. |
| `user@host:cwd` header | On by default, mirroring the familiar shell prompt — useful when you have sessions open on several machines. |
| Pills panel | <kbd>ctrl+t</kbd> expands *every* section, and scheduled tasks get their own pill with a stopwatch glyph that distinguishes recurring from one-shot. |

### Configuration

| Improvement | Detail |
| --- | --- |
| `allowed_commands` | Re-allow specific commands the [blocklist](/configuration/permissions#blocked-commands) normally denies, without disabling the blocklist. Still subject to permission approval. |
| `channel_enabled` | Turn an MCP server into a channel from `crush.json`, rather than only via the `--channels` flag. |
| `embeddings` block | Configure the semantic-search embedding provider from `crush.json` or `crushrc`. |
| Config reload on reconnect | Reconnecting an MCP server re-reads config from disk, so an edit takes effect without a restart. |
| Current time in the prompt | The system prompt carries the current time, which is what makes relative scheduling ("every weekday at 9") work. |
| `a2ui` built-in skill | Teaches the model the A2UI contract, including the MCP resource-read path. |
| Built-in skills in the palette | All built-in skills are marked `user-invocable`, so they appear in <kbd>ctrl+p</kbd> as `user:` entries. |

## Fixes this fork carries

Fixes to **upstream** behaviour that live here and are not upstream. Fixes to
the fork's own features are part of those features and are not listed
separately.

| Fix | Symptom it removes |
| --- | --- |
| `--yolo` made a persistent flag | `crush --yolo run …` died with `unknown flag`, which crash-looped anything invoking a subcommand. |
| Bounded MCP init waits | A wedged MCP server could blank the whole app at startup. Tool lists now wait for init, with a bound. |
| Per-server MCP lifecycle serialized | Concurrent connect/disconnect on one server raced; an error on one session tore down others. |
| stdio process groups reaped | Killed stdio MCP servers left orphan processes behind. |
| MCP OAuth persistence | Tokens did not actually survive a restart; the store now does atomic writes and validates state. |
| In-process run dispatch serialized | Two turns could run concurrently in one session. |
| Provider base URL resolved before validation | A custom provider with an expanded `base_url` failed validation that should have passed. |
| Empty paste routes to the clipboard image | An empty bracketed paste dropped the image instead of attaching it — and now warns instead of silently failing on a model that cannot take images. |
| Command filter matches shortcuts | Typing a command's shortcut in the `/` menu filtered it out. |
| Tied project timestamps ordered by recency | Projects registered in the same instant sorted arbitrarily. |
| Backspace in the commands filter | Backspace was swallowed in the top-level commands filter. |

## What this fork does *not* add

Things regularly mistaken for fork features. All of these are upstream Crush,
built by [Charm](https://charm.land) — bugs in them belong
[upstream](https://github.com/charmbracelet/crush/issues).

| Upstream feature | Where it lives |
| --- | --- |
| **Background jobs** | `bash` backgrounding long-running commands, plus `job_output` and `job_kill`. Upstream since [charmbracelet/crush#1328](https://github.com/charmbracelet/crush/pull/1328). See [tools](/reference/tools#shell-and-job-control). |
| **Clipboard image paste** | <kbd>ctrl+v</kbd> attaching an image, with the clipboard-text-as-path fallback. Upstream; the fork only fixed the empty-paste case above. |
| **Channel push** | The `claude/channel` MCP capability and the `--channels` flag are upstream. Only the routing, config key, and reply tool are the fork's. |
| **MCP OAuth** | HTTP MCP servers authenticating over OAuth. Now upstream; the fork carries hardening fixes on top. |
| **Hooks, Skills, `crushrc`, LSP, sessions, workspaces, notifications, `/clear`, `/compact`, the pills panel, the sidebar itself** | All upstream. |

## Landed but not yet wired up

| Thing | Status |
| --- | --- |
| A2A (`internal/a2a`) | Agent Card and `AgentExecutor` foundation on the [a2a-go](https://github.com/a2aproject/a2a-go) SDK. Compiled but not yet reachable from the CLI or config — no flag, no config key, no docs page. Do not plan around it yet. |

## Reporting bugs

Crush is licensed
[FSL-1.1-MIT](https://github.com/charmbracelet/crush/raw/main/LICENSE.md).

- Anything on this page → [`joestump-agent/crush` issues](https://github.com/joestump-agent/crush/issues)
- Anything else → [`charmbracelet/crush` issues](https://github.com/charmbracelet/crush/issues)
