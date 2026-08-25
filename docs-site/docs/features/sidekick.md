---
id: sidekick
title: Sidekick
sidebar_position: 5
description: A second, ephemeral agent in the sidebar with read-only tools, plus a pinned dashboard the main agent pushes live status to.
---

# Sidekick

:::info[Fork feature]
Sidekick is an addition in the `joestump-agent/crush` fork. It is an MVP; the
shape may change.
:::

The Sidekick is a second agent living in the sidebar. It exists so you can ask a
question — *what does this package do?*, *where is that config read?* — without
interrupting the main coder agent mid-task.

Press <kbd>ctrl+a</kbd> to toggle it, or <kbd>tab</kbd> to cycle sidebar focus
between `[Info]` and `[Sidekick]`.

## Two independent streams

The panel carries two things that look similar but are not related:

1. **The Sidekick chat** — your conversation with the second agent.
2. **The dashboard** — a pinned A2UI surface at the top of the panel that the
   **main** agent pushes to.

## The chat

| Property | Behaviour |
| --- | --- |
| Session | Ephemeral and in-memory. Nothing is persisted; closing Crush discards it. |
| Model | The **small** model by default |
| Tools | Read-only (see below) |
| Traffic isolation | Sidekick messages can never enter the main chat's history, busy state, or queue |

Commands inside the panel:

| Command | Does |
| --- | --- |
| `/clear` | Wipe the ephemeral conversation and start fresh |
| `/model` | Open a Sidekick-scoped model picker — session-scoped, never persisted, never touches the coder agent's model |
| <kbd>esc</kbd> | Cancel the running Sidekick turn |

An unread badge appears on the tab when the Sidekick produces output while you
are looking at `[Info]`.

### Read-only tools

The Sidekick gets a deliberately small, non-mutating tool set:

- A **read-only `bash`** variant, which statically filters commands by walking
  the shell AST rather than pattern-matching the string
- `glob`, `grep`, `ls`, `view`
- `sourcegraph`

It gets no MCP tools, no LSP tools, no todo tool, and nothing that writes. It is
also never wrapped with [`PreToolUse` hooks](/features/hooks) — there is nothing
for them to gate.

## The dashboard

The main coder agent has a `sidekick_update` tool that pushes an A2UI
`updateComponents` payload onto a dedicated channel. The payload renders as a
pinned, **fully interactive** surface at the top of the Sidekick panel.

This is a side channel for machine-readable UI: the surface renders only in the
dashboard and never appears in the chat transcript.

What it is for:

- Progress on multi-step tasks — a progress readout, a status card, a checklist
- A compact live status you can glance at while the agent keeps working

How it behaves:

- Each call **replaces** the previous surface in place. Reusing the same
  `surfaceId` and component ids makes rapid updates (20% → 40% → 60%) redraw
  smoothly instead of accumulating UI.
- The dashboard persists after the turn ends, until your next prompt or until
  you dismiss it with <kbd>ctrl+x</kbd>.
- <kbd>shift+tab</kbd> focuses the dashboard surface so you can interact with it.
- Pressing a button retires the form, collects the field values, and starts a
  new **main-agent** turn. A cancel button just unpins.
- The tool returns immediately; it never blocks the agent's turn.

`sidekick_update` is registered only for the top-level coder agent — never for
sub-agents, never for the Sidekick itself — and only when a Sidekick panel is
configured and [A2UI](/features/a2ui) is enabled.

## Keys

| Keys | Does |
| --- | --- |
| <kbd>ctrl+a</kbd> | Toggle the Sidekick panel |
| <kbd>tab</kbd> | Cycle sidebar focus |
| <kbd>shift+tab</kbd> | Focus the pinned dashboard surface |
| <kbd>ctrl+x</kbd> | Dismiss the dashboard |
| <kbd>esc</kbd> | Cancel the running Sidekick turn |

## Limits

- The Sidekick conversation is not persisted and does not appear in the session
  list.
- Inline A2UI in the Sidekick *chat* is display-only; only the *dashboard*
  surface is interactive.
- The Sidekick model selection is session-scoped. To change the default,
  change your [small model](/configuration/command-reference#model-large-model-small).
