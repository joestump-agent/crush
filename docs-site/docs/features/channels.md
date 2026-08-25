---
id: channels
title: Channels
sidebar_position: 3
description: MCP servers that push real-time events into your session — CI failures, webhooks, chat messages — and how Crush replies back through them.
---

# Channels

:::info[Fork feature]
Channels are an addition in the `joestump-agent/crush` fork. They are
experimental and the shape may change.
:::

An MCP server can act as a **channel**: instead of only exposing tools that
Crush calls, it *pushes* events straight into your session, so Crush reacts to
things happening outside the terminal — a webhook, a CI failure, a chat message.

The wire protocol is Claude's
[channels reference](https://code.claude.com/docs/en/channels-reference).

## How a server becomes a channel

A server declares the `claude/channel` capability in its `initialize` result:

```json
{ "capabilities": { "experimental": { "claude/channel": {} } } }
```

…and then emits `notifications/claude/channel` events with a `content` body and
an optional `meta` map. Crush injects each event into the active session as a
`<channel>` element:

```text
<channel source="webhook" severity="high" run_id="1234">
build failed on main: https://ci.example.com/run/1234
</channel>
```

## Opting in

**Listing a channel server in `mcp` is not enough.** Pushing is gated behind an
explicit opt-in, so a configured server stays silent until you ask for it.

Per launch:

```bash
crush --channels server:webhook
crush --channels server:webhook --channels server:signal
```

Or persistently, with `channel_enabled` on the server's `mcp` entry:

```json
{
  "mcp": {
    "webhook": {
      "type": "http",
      "url": "https://example.com/mcp",
      "channel_enabled": true
    }
  }
}
```

Either source enables the channel — but the server must still declare the
capability. Servers that are live channels are marked `channel` in the MCP list.

## Reply routing

By default a channel-originated turn only produces terminal output, so a person
messaging you on Signal never sees the answer unless the model happens to call a
send tool itself.

A `channel_reply` block makes the routing deterministic: when a turn that
originated from that channel finishes **without** the model having replied
through the channel, Crush sends the final assistant response back through the
configured tool — to the sender for direct messages, or to the group for group
messages.

```json
{
  "mcp": {
    "signal": {
      "type": "stdio",
      "command": "uv",
      "args": ["run", "signal_mcp/main.py", "--operator", "+15551234567", "--channel"],
      "channel_reply": {
        "user":  { "tool": "send_message_to_user",  "target_param": "user_id" },
        "group": { "tool": "send_message_to_group", "target_param": "group_id" },
        "suppress_tools": ["send"]
      }
    }
  }
}
```

How it routes:

- **Group pushes** (meta carries `group`) go through the `group` route;
  **direct pushes** (meta carries `sender`) go through the `user` route.
- `target_meta` overrides which meta attribute supplies the target.
- `message_param` (default `message`) names the tool argument that receives the
  reply text.
- If the model already called a route tool — or anything listed in
  `suppress_tools` — during the turn, the automatic reply is skipped, so richer
  model-driven replies aren't duplicated.
- Local (non-channel) turns, and channels without a `channel_reply` block, are
  unaffected.

The same shape works for any messaging channel — Discord, Slack, whatever —
point the routes at that server's send tools and the matching meta attributes.

## Two-way channels

A channel is a regular MCP server, so any tool it exposes is available to the
agent through the normal MCP tool path. Nothing channel-specific is required to
make a channel interactive:

1. Declare `tools` in the server's capabilities and register a `reply` tool.
2. Use the server's `instructions` string — injected into the system prompt — to
   tell Crush when to call it and which `<channel>` attribute to pass back
   (a `chat_id`, say).

`channel_reply` and a model-driven reply tool compose: the deterministic reply
fires only when the model didn't reply itself.

## Security model

Channel payloads are **untrusted, server-initiated input**, and Crush treats
them that way:

- The `source` attribute is always the (trusted) server name — a payload cannot
  forge it.
- Crush validates payload structure, caps the body and attribute sizes,
  restricts `meta` keys to identifiers (`[A-Za-z0-9_]`), and escapes all content
  so a payload cannot break out of the `<channel>` element or forge attributes.
- Malformed payloads are dropped.
- A server that has not been opted in via `--channels` / `channel_enabled`, or
  that never declared the capability, cannot inject anything at all.

:::warning
Everything inside a `<channel>` element is data, not instruction. Anyone who can
reach the pushing service can put words in front of your agent — treat channel
content the way you would treat a web page, and keep dangerous tools behind
[permissions](/configuration/permissions).
:::

## Delivery semantics

Channel delivery works both in the default in-process `crush` and against a
shared [`crush server`](/features/server-and-workspaces) backend.

**In-process.** An event routes into the session you have open, or starts one if
none is open, so it is never dropped.

**Against a server.** The server routes each event exactly once: into the
session an attached client is viewing (the most recently updated one when
clients are viewing different sessions), otherwise into the workspace's most
recent session, creating one only when none exists. That holds even with no
clients connected, so a headless server still processes channel pushes; attached
clients see the injected turn arrive through the normal event stream.

## Worked example: Signal

[Signal MCP](https://github.com/joestump/signal-mcp) lets Crush send and receive
Signal messages through a
[signal-cli](https://github.com/AsamK/signal-cli) daemon.

1. **Start the daemon:**

   ```bash
   signal-cli -a +15551234567 daemon --tcp 127.0.0.1:7583 \
     --receive-mode on-start --no-receive-stdout
   ```

2. **Configure the server** using the `crush.json` example in
   [MCP servers](/features/mcp#adding-a-server), adding the `channel_reply`
   block above.

3. **Launch with the channel enabled:**

   ```bash
   crush --channels server:signal
   ```

Incoming messages arrive as `<channel>` tags; replies go back out through
`send_message_to_user` — either because the model called it, or because
`channel_reply` routed the final response there.
