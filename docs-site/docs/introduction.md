---
id: introduction
title: Introduction
sidebar_label: Introduction
sidebar_position: 1
description: What Crush is, what this fork adds, and where to start reading.
---

# Crush

**Your new coding bestie, now available in your favourite terminal.** Your
tools, your code, and your workflows, wired into your LLM of choice.

Crush is a terminal-first coding agent from [Charm](https://charm.land). It
runs where you already work, talks to whichever model you want, reads your code
through language servers, and extends through MCP servers and Agent Skills.

## What Crush does

- **Multi-model.** Dozens of providers out of the box, plus anything speaking an
  OpenAI- or Anthropic-compatible API — including local models.
- **Flexible.** Switch models mid-session without losing context.
- **Session-based.** Multiple work sessions and contexts per project.
- **LSP-enhanced.** Crush uses language servers for extra context, just like
  you do.
- **Extensible.** MCP servers over `http`, `stdio`, and `sse`; Agent Skills from
  disk.
- **UI-fluent.** Models can speak [A2UI](https://a2ui.org) and Crush draws it —
  cards, lists, buttons, and dashboards rendered right in the chat.
- **Channel-ready.** MCP servers can push real-time events into your session —
  CI failures, webhooks, chat messages — and Crush acts on them without you
  typing a thing.
- **Works everywhere.** First-class support in every terminal on macOS, Linux,
  Windows (PowerShell and WSL), Android, FreeBSD, OpenBSD, and NetBSD.
- **Industrial grade.** Built on the Charm ecosystem, powering 25k+
  applications.

## About this fork

These docs cover **[`joestump-agent/crush`](https://github.com/joestump-agent/crush)**,
a fork that tracks [`charmbracelet/crush`](https://github.com/charmbracelet/crush)
upstream and adds features on top. Everything upstream does, this fork does.

Pages that document a fork addition are marked with a note like this one:

:::info[Fork feature]
This section documents behaviour added by the `joestump-agent/crush` fork. It is
not present in upstream Crush.
:::

The additions, in brief:

| Addition | What it is |
| --- | --- |
| [A2UI](/features/a2ui) | Inline rendering of A2UI surfaces via [a2tea](https://github.com/joestump-agent/a2tea), including interactive round-trips |
| [Scheduled tasks](/features/scheduled-tasks) | `CronCreate` / `CronList` / `CronDelete` tools that re-run prompts on a cron schedule |
| [Semantic search](/features/semantic-search) | A local sqlite-vec index over the repo, searched by meaning |
| [Channels](/features/channels) | The push mechanism is upstream; the fork adds the `channel_enabled` config key, the `channel_reply` tool, and deterministic reply routing |
| [MCP prompts](/features/mcp#prompts-and-resources) | MCP prompts offered in the `/` completions and the command palette |

Plus a long tail of quality-of-life work — click-to-copy, clickable
hyperlinks, a syntax-highlighted composer, text-file attachments, Skills, MCP
and Channels dialogs, a focusable sidebar — and a set of fixes to upstream
behaviour.

**[What this fork adds](/fork)** is the complete, verified inventory, including
a list of things commonly mistaken for fork features that are in fact upstream.

Crush is licensed
[FSL-1.1-MIT](https://github.com/charmbracelet/crush/raw/main/LICENSE.md).
Bugs in upstream behaviour belong
[upstream](https://github.com/charmbracelet/crush/issues); bugs in the additions
above belong [here](https://github.com/joestump-agent/crush/issues).

## Where to start

1. **[Installation](/getting-started/installation)** — a package manager line
   for your platform.
2. **[Quickstart](/getting-started/quickstart)** — first run, picking a model,
   and the keys you need.
3. **[Providers and API keys](/getting-started/providers-and-api-keys)** — the
   full environment-variable table.
4. **[Configuring Crush](/configuration/crushrc)** — `crushrc`, the Bash config
   format, and where config lives.

If you just want to look something up, the
**[CLI reference](/reference/cli)**, the
**[tool reference](/reference/tools)**, and the
**[config command reference](/configuration/command-reference)** are the flat
lookup pages.

:::tip[Ask Crush about Crush]
Crush ships a built-in `crush-config` skill. Most of the time you can describe
what you want configured in plain English and Crush will write the config
itself. See [Skills](/features/skills#built-in-skills).
:::
