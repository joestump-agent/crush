---
id: a2ui
title: A2UI
sidebar_position: 4
description: Models speak A2UI and Crush draws it — cards, lists, buttons, and interactive forms rendered inline in the chat.
---

# A2UI

:::info[Fork feature]
Inline A2UI rendering, MCP resource surfaces, and interactive round-trips are
additions in the `joestump-agent/crush` fork.
:::

Sometimes prose isn't the best answer. When an assistant reply contains an
[A2UI](https://a2ui.org) message — structured JSON describing UI, wrapped in
`<a2ui-json>` tags — Crush renders it as an actual element in the chat instead
of dumping raw JSON:

```text
<a2ui-json>{"version":"v0.9","updateComponents":{...}}</a2ui-json>

        …becomes…

╭──────────────────────────────╮
│ Build passed                 │
│ 142 tests, 0 failures.       │
│ ──────────────────────────── │
│ [ Details ]  [ Re-run ]      │
╰──────────────────────────────╯
```

Parsing and rendering are handled by
[a2tea](https://github.com/joestump-agent/a2tea), which speaks the real A2UI
v0.9 protocol. Chrome is monochrome on purpose, so your theme stays yours.

## Two delivery paths

**1. An MCP `/a2ui` resource.** Servers that own data — a queue, a trace, a
dashboard — expose resource templates whose URI ends in `/a2ui`. Reading one
renders the surface for you and hands the model only a placeholder, so the JSON
never enters its context:

```text
read_mcp_resource(mcp_name="todos", uri="todos://queue/backlog/a2ui")
```

This is the richer, cheaper path: server-rendered, sized to the host.

**2. An inline `<a2ui-json>` block.** For ad-hoc visuals no server provides, the
model embeds one `updateComponents` message in its reply. This is the
chat-scanned path — good for summaries and one-shot forms the model authors
itself.

## The component catalog

Components live in a flat adjacency list and reference children by `id`. The
renderer resolves the tree from the root — the component nothing else names as a
child.

**Core, fully drawn:**

| Component | Notes |
| --- | --- |
| `Text` | `variant` of `h1`–`h5` or `caption`; omit for body |
| `Card` | Rounded-border container, single `child` |
| `Column` / `Row` | Vertical / horizontal layout over `children` |
| `List` | Children as rows with dividers |
| `Divider` | A rule |
| `Button` | Focusable. Its label is a child `Text` id — there is no `label` field |

**Input components** — `TextField`, `CheckBox`, `ChoicePicker`, `Slider`,
`DateTimeInput` — render editable and return their values keyed by component
`id` when a button on the surface is pressed.

**Placeholders** — `Image`, `Icon`, `Video`, `AudioPlayer` render as compact
one-liners; `Tabs` renders the title bar plus the first tab; `Modal` renders
only its trigger.

## Forms and the submission loop

A surface with inputs and a `Button` is a form. Pressing the button sends the
button's identity and the current field values back to the model, which starts a
new turn. A button whose id reads as a cancel (`btn-cancel`, `dismiss`, `close`)
just dismisses the surface without submitting.

```json
<a2ui-json>{
  "version": "v0.9",
  "updateComponents": {
    "surfaceId": "form",
    "components": [
      {"component": "Card", "id": "root", "child": "col"},
      {"component": "Column", "id": "col", "children": ["name", "email", "actions"]},
      {"component": "TextField", "id": "name", "label": "Name"},
      {"component": "TextField", "id": "email", "label": "Email"},
      {"component": "Row", "id": "actions", "children": ["btn-send", "btn-cancel"]},
      {"component": "Button", "id": "btn-send", "child": "btn-send-label"},
      {"component": "Text", "id": "btn-send-label", "text": "Send"},
      {"component": "Button", "id": "btn-cancel", "child": "btn-cancel-label"},
      {"component": "Text", "id": "btn-cancel-label", "text": "Cancel"}
    ]
  }
}</a2ui-json>
```

## A2UI over MCP

When a surface comes from a server, the
[A2UI-over-MCP contract](https://a2ui.org/guides/a2ui_over_mcp/) governs:

- **MIME type** `application/a2ui+json` (legacy `application/json+a2ui`)
  identifies a surface, delivered by `resources/read` on an `/a2ui` resource, or
  by a `tools/call` result carrying an `EmbeddedResource` of that type.
- **Audience.** A surface annotated `["user"]` renders for the human and its raw
  JSON is kept out of the model's context. `["assistant"]` is for the model's
  reasoning and is not rendered. Empty means both.
- **Interactivity round-trip.** A server surface's `Button` carries an
  `action` of `{event: {name, context}}`. On click, Crush calls the owning
  server's `a2ui_action` tool with the resolved data bindings and feeds any
  returned payload back into the **same** surface — with no agent turn at all.
  Render failures go back via `a2ui_error`.
- **Width hint.** A template declaring `{?w}` accepts an optional `?w=N` sizing
  hint, which the host appends. Nothing else should append one.

Crush advertises `capabilities.a2ui` at MCP `initialize`, so a server can detect
that it is talking to an A2UI-capable host before offering surfaces.

## Code, Markdown, and syntax highlighting

Body-variant `Text` — a `Text` with no `variant` — is passed through Crush's
Markdown renderer (glamour, with chroma for code). So a surface is not limited
to flat strings: tables, lists, bold, inline code, and **syntax-highlighted
fenced code blocks** all render inside a card.

```json
{"component": "Text", "id": "snippet", "text": "```go\n42  func Verify() error {\n43      return nil\n44  }\n```"}
```

The fence's language tag selects the lexer, so tag it. Line numbers are simply
part of the block's text — the gutter is lexed along with the code and reads as
a distinct color. Long lines word-wrap to column zero, which breaks that gutter,
so a producer drawing one should truncate instead.

The heading and caption variants deliberately skip the Markdown renderer: they
are short labels, and their variant style already handles them.

## Semantic search results

`semantic_search` and `semantic_index` use all of this. When a chat UI is
attached, their results divert into a surface and the model keeps a compact
text digest — so the snippets cost the user nothing in context:

```text
╭───────────────────────────────────────────────────────╮
│ Semantic search                                       │
│ "where is auth handled" · 3 results                   │
│ 1. internal/auth/token.go:42                          │
│ Verify · lines 42–89 · ███████░░░ 0.734               │
│                                                       │
│   42  func Verify() error {                           │
│   43      return nil                                  │
│   44  }                                               │
│       …                                               │
│                                                       │
│ ───────────────────────────────────────────────────── │
│ 2. …                                                  │
╰───────────────────────────────────────────────────────╯
```

Each hit carries a rank, a `path:line` jump target relativized to the working
directory, the symbol, the chunk's line range, a fixed-width relevance meter
beside the score, and the chunk's opening lines with a line-number gutter and
syntax highlighting. Snippet lines are sized from the pane's width hint so the
gutter survives.

Headless and channel-originated runs are unaffected — with no chat UI to render
metadata, both tools emit their original plain text.

## Where surfaces render

| Location | Behaviour |
| --- | --- |
| Chat transcript | Full rendering, interactive |

## Turning it off

```json
{ "options": { "disable_a2ui": true } }
```

With A2UI disabled, `<a2ui-json>` blocks are left as text and the

## Notes and limits

- Prose around a surface still renders as Markdown.
- A block that fails to parse shows an alert rather than silently disappearing.
- Every `child` / `children` id must exist — a dangling id renders
  `[a2tea: missing component …]`.
- Keep surfaces compact. A surface is a summary, not a viewer — show a handful
  of lines of code and point at the file for the rest.

## The built-in skill

Crush ships an `a2ui` skill that teaches the model both delivery paths, the
component catalog, and the MCP contract. It is invocable from the command
palette as well. See [Skills](/features/skills#built-in-skills).
