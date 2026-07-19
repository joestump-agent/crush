Pushes a live A2UI status surface to the Sidekick dashboard — the pinned slot at the top of the sidebar's Sidekick tab. This is a side channel for machine-readable UI: the surface renders only in the dashboard and never appears in the chat transcript.

WHEN TO USE
- Show progress on multi-step tasks: a progress readout, a status card, a checklist of steps completed so far.
- Surface a compact live status the user can glance at while you keep working.

HOW TO USE
- Pass one A2UI `updateComponents` payload as a JSON string — the same object you would emit inline inside an `<a2ui-json>` block. Do not include the tags.
- Each call REPLACES the previous dashboard surface in place. Reuse the same surfaceId and component ids so rapid updates (20% → 40% → 60%) redraw smoothly instead of accumulating UI.
- The dashboard persists after your turn ends, until the user's next prompt or until they dismiss it.
- Keep inline `<a2ui-json>` blocks for content that belongs in the conversation itself.

Renderable components: Text (variants h1-h5, caption), Card, Column, Row, List, Divider, Button; input components render read-only. Never put code in a surface.

The tool returns "rendered" immediately; it never blocks your turn.
