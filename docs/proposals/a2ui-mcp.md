# Proposal: expanding Crush's A2UI integration — theming bridge and A2UI over MCP

Status: draft, for discussion. Companion doc: `docs/proposals/styling-layers.md`
in a2tea, which specifies the renderer-side style token work this depends on.

Current state (feat/a2ui): the coder prompt advertises the A2UI v0.9 basic
catalog, assistant output is scanned for `<a2ui-json>` blocks
(`internal/ui/chat/a2ui.go`), surfaces render inline via a2tea with focus/key
routing, and button presses retire the surface and become a new agent turn
(`A2UISubmissionPrompt` → `sendMessage`). Surfaces render monochrome inside the
themed `Messages.A2UISurface` container.

Four expansion threads, in rough priority order.

## 1. Theme bridge: Crush palette → a2tea styles

A2UI's styling layers put the *host* in charge of the final look (the web analog
is overriding `--a2ui-*` CSS variables). Crush already has a semantic palette —
`quickStyleOpts` in `internal/ui/styles/quickstyle.go` (primary/secondary/accent,
fg/bg ramps, statuses) — expanded into `styles.Styles`. A2UI surfaces are the
only part of the chat that ignores it.

Plan:

- Add an `A2UI a2tearender.Styles` group to `styles.Styles`, populated inside
  `quickStyle()` from the semantic palette. Sketch of the mapping:

  | a2tea slot | Crush palette |
  | --- | --- |
  | `CardBorder` | border style used by `Messages` blocks (bgLessVisible border) |
  | `Heading` / `Subheading` | fgBase bold / fgSubtle bold |
  | `Caption`, `Label`, `Placeholder` | fgMoreSubtle / fgSubtle / fgMostSubtle |
  | `Button` / `ButtonFocused` | secondary / onPrimary-on-primary |
  | `ButtonPrimary(-Focused)` | accent (this is what finally gives `variant: primary` a face) |
  | `InputFocused`, focus-cue glyph | primary |
  | `Divider`, `ListBullet`, `TabActive/Inactive` | separator / fgSubtle / fgBase |

- Pass it at surface build: `a2tea.Render(p.Messages,
  render.WithStyles(a.sty.A2UI))` in `syncA2UISurfaces`
  (`internal/ui/chat/a2ui.go:569`).
- Live theme swap: `refreshStyles` (`internal/ui/model/ui.go:3770`) currently
  invalidates markdown caches; surfaces must be restyled **without rebuilding**
  (rebuilding would wipe typed field values). This needs a2tea's proposed
  `Surface.SetStyles`; until it exists, restyle only non-retired surfaces by
  rebuild and accept the state loss, or gate the bridge behind the SetStyles
  landing. Prefer waiting for SetStyles — theme swaps mid-form are rare but
  losing a half-filled form to one is nasty.
- Defaults unchanged for other a2tea hosts: monochrome remains a2tea's default;
  the bridge is Crush opting in, exactly as the layering model intends.

## 2. A2UI over MCP: Crush as an A2UI-capable MCP host

The spec's A2UI-over-MCP guide defines a transport-level contract that fits
Crush's existing MCP client (official `modelcontextprotocol/go-sdk`, tools +
prompts + resources already wired):

- **MIME type**: `application/a2ui+json` (accept legacy `application/json+a2ui`
  too — the reference clients check both) identifies an A2UI payload wherever it
  appears.
- **Static UI as resources**: servers list resources like `a2ui://recipe-form`;
  reading one returns A2UI JSON.
- **Dynamic UI from tools**: a `CallToolResult` carries an `EmbeddedResource`
  with that MIME type (plus fallback `TextContent` for non-A2UI clients).
- **Capability negotiation**: the client declares
  `capabilities.a2ui.clientCapabilities.v0.9.supportedCatalogIds` at MCP
  `initialize` (or per-call `_meta` for stateless servers).
- **Actions back**: interactions become `a2ui_action` tool calls
  (`{name, context}` with bindings resolved against the surface data model);
  render failures become `a2ui_error` calls.
- **Visibility**: resource `annotations.audience` controls whether the payload is
  rendered for the user, shown to the LLM, or both.

Concretely in Crush:

1. **Detect** — in the MCP tool-call result path (`internal/agent/tools/mcp/`)
   and in `read_mcp_resource`, check embedded/resource contents for the A2UI
   MIME type. On match, hand the JSON to the existing repair + `a2tea.Render`
   pipeline instead of dumping it as text into the tool output.
2. **Render** — MCP-originated surfaces need a home in the chat list. Simplest:
   render inside the tool-call message item, reusing the assistant item's
   surface machinery (extract it from `AssistantMessageItem` into a shared
   `surfaceHost` embedded by both). Track provenance: `surfaceID → MCP client
   name` so events route back to the right server.
3. **Advertise** — set the `a2ui` capability at client init
   (`internal/agent/tools/mcp/init.go:492`) with the compiled-in catalog ID.
   This also tells well-behaved servers not to send A2UI to hosts that can't
   render it.
4. **Round-trip actions** — on `ButtonClicked` for an MCP-owned surface: if the
   owning server exposes `a2ui_action`, call it with `{name, context}` and feed
   any A2UI payload in the response back into the same surface (update, not
   retire — MCP surfaces are long-lived apps, unlike one-shot chat forms). If it
   doesn't, fall back to today's behavior (retire + `A2UISubmissionPrompt` as an
   agent turn). Report render failures via `a2ui_error` when present.
5. **Audience** — respect `annotations.audience`: `["user"]` renders the surface
   but keeps the raw JSON out of the conversation context sent to the model.
6. **@-mentions** — resource completions already exist
   (`internal/ui/completions/completions.go:419`); an `a2ui://` resource
   mentioned in a prompt should render as a surface rather than paste JSON.

This makes Crush (via a2tea) the first *terminal* A2UI-over-MCP host, and it's
mostly plumbing — the renderer, resource reading, and event types all exist.

## 3. Questions: converge with upstream v0.85.0

Upstream Crush v0.85.0 shipped a first-class question tool (single choice,
multiple choice, free-form, yes/no, gangable into forms). This fork predates it
— there is no question tool here; A2UI forms are our answer to the same problem.
Two differences worth closing:

- **Delivery**: upstream questions are *tool-driven* (the model calls a tool,
  the turn blocks, the answer returns as the tool result). Our A2UI forms are
  *markup-driven* (prose tag scan) and answers come back as a synthetic user
  turn (`[A2UI form submission] …`), which burns a turn and reads oddly in
  history.
- **Proposal**: add a native `question` tool whose implementation renders an
  A2UI surface (built with `tmc/a2ui`'s `a2uibuild`) through the same chat
  machinery, blocks the agent turn, and returns the field values as the tool
  result. The A2UI markup path stays for free-form generative UI; the tool path
  covers the structured ask-the-user case with upstream-compatible semantics.
  Bonus: the same canonical form surfaces (single/multi choice, free text,
  yes/no, ganged form) become a reusable template library — see §4.

## 4. Crush components as A2UI resources

The inverse direction: expose Crush's common UI patterns *as* A2UI. Two stages:

- **Template library first** — canonical A2UI JSON for the question forms,
  permission prompt, todo checklist, model picker. Ship them as embedded
  resources (like `crush://skills/a2ui/SKILL.md`) usable by the `question` tool,
  by the skill as worked examples, and by tests as goldens.
- **MCP server later (exploratory)** — Crush has an HTTP API server
  (`internal/server`) but no MCP server. Adding one that lists the template
  library as `a2ui://crush/...` resources (MIME `application/a2ui+json`) and
  exposes `a2ui_action` would let any A2UI-capable host render Crush-style
  prompts, per the spec's resource/tool delivery contract. Do this only once §2
  proves the contract from the client side; it's the same payloads either way.

## 5. MCP Apps: fallback-only

The `McpApp` component is sandboxed HTML in a double iframe — there is no
terminal equivalent, and we should not fetch or execute HTML. Posture: when a
surface references an unknown/custom component like `McpApp`, render an honest
placeholder (title + "interactive app not supported in terminal"), and rely on
the spec's fallback-text convention for tool results. Revisit only if a
"open in browser" affordance ever seems worth it.

## Sequencing

1. a2tea: `obscured` fix, `Styles`/`Glyphs` expansion, `SetStyles` (companion doc).
2. Crush: theme bridge (§1) — bump a2tea, map palette, wire theme swap.
3. Crush: A2UI-over-MCP client (§2) — detect, render, capability, `a2ui_action`.
4. `question` tool + template library (§3, §4 stage 1).
5. Exploratory: Crush MCP server (§4 stage 2), McpApp placeholder polish (§5).

Housekeeping alongside: README's A2UI section still says interactivity is "on
the roadmap" (it shipped), and the a2ui skill should grow a line about themed
rendering once §1 lands.
