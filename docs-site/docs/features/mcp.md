---
id: mcp
title: MCP servers
sidebar_position: 2
description: Extend Crush over stdio, HTTP, and SSE — including OAuth, tool filtering, prompts, and resources.
---

# MCP servers

Crush speaks the [Model Context Protocol](https://modelcontextprotocol.io) over
three transports: `stdio` for command-line servers, `http` for HTTP endpoints,
and `sse` for Server-Sent Events.

## Adding a server

```bash
# crushrc

# A local MCP server that runs a Node.js script.
mcp add filesystem --command node --args /path/to/mcp-server.js \
  --timeout 10 --disabled-tools some-tool-name --env NODE_ENV production

# A GitHub MCP server that uses an API token.
mcp add github --type http --url "https://api.githubcopilot.com/mcp/" \
  --timeout 10 --header Authorization "Bearer $GH_PAT" \
  --disabled-tools create_issue --disabled-tools create_pull_request

# A streaming MCP server that uses SSE.
mcp add streaming-service --type sse --url "https://example.com/mcp/sse" \
  --timeout 10 --header API-Key "$API_KEY"
```

The equivalent in [JSON](/configuration/json):

```json
{
  "$schema": "https://charm.land/crush.json",
  "mcp": {
    "signal": {
      "type": "stdio",
      "command": "uv",
      "args": [
        "run", "--directory", "/path/to/signal-mcp",
        "python", "signal_mcp/main.py",
        "--user-id", "+15551234567", "--channel"
      ]
    }
  }
}
```

### Empty headers are dropped

Headers whose value resolves to the empty string — MCP `headers` and provider
`extra_headers` alike — are dropped from the outgoing request rather than sent
as a bare `Header:`. That keeps optional env-gated headers clean when the
variable is unset.

## Tool names

MCP tools reach the agent as `mcp_<server>_<tool>`. That is the name to use in
`permissions allow` / `permissions deny`:

```bash
permissions allow mcp_context7_get-library-doc
```

## Restricting tools

Two mutually complementary flags on the server:

```bash
# Deny specific tools.
mcp add github --type http --url "…" \
  --disabled-tools create_issue --disabled-tools create_pull_request

# Or allow only these, denying everything else.
mcp add github --type http --url "…" \
  --enabled-tools search_code --enabled-tools get_file_contents
```

Disable a whole server without removing its config with `--disabled true`.

## OAuth

HTTP and SSE servers that need OAuth can use Crush's built-in
authorization-code flow instead of a static `Authorization` header:

```json
{
  "mcp": {
    "linear": {
      "type": "http",
      "url": "https://mcp.linear.app/mcp",
      "oauth": true
    }
  }
}
```

Crush attempts dynamic client registration (RFC 7591), which works with Linear,
Notion, and others.

### Pre-registered clients

Some servers (GitHub, Slack) don't support dynamic registration. Register an
OAuth app with the provider and supply the credentials — all values support
shell expansion:

```json
{
  "mcp": {
    "github": {
      "type": "http",
      "url": "https://api.githubcopilot.com/mcp/",
      "oauth": true,
      "oauth_client_id": "Iv1.abc123def456",
      "oauth_client_secret": "$GITHUB_MCP_SECRET",
      "oauth_callback_port": 40704
    }
  }
}
```

When `oauth_client_id` is set, Crush skips dynamic registration and
authenticates as that client. `oauth_callback_port` pins the localhost port the
callback lands on, which matters when the provider requires a fixed redirect
URI.

## Prompts and resources

Beyond tools, Crush surfaces the other two MCP primitives.

**Resources.** The agent can list and read them:

| Tool | Does |
| --- | --- |
| `list_mcp_resources` | List resource URIs and templates from a server |
| `read_mcp_resource` | Read a resource by URI |

Resources whose URI ends in `/a2ui` render as
[A2UI surfaces](/features/a2ui) rather than raw text.

**Prompts.** The agent can call them, and so can you:

| Tool | Does |
| --- | --- |
| `list_mcp_prompts` | List the prompts a server exposes |
| `call_mcp_prompt` | Invoke a prompt and get its rendered content |

:::info[Fork feature]
This fork also offers MCP prompts in the inline `/` completions and loads them
at startup, so a server's prompts are reachable from the composer as soon as the
session opens.
:::

These four tools are only registered when you have at least one MCP server
configured.

## Server instructions

A server's `instructions` string from its `initialize` result is injected into
the system prompt. That is the right place for a server to explain when its
tools should be used — and it is what makes a two-way
[channel](/features/channels#two-way-channels) work without any
channel-specific plumbing.

## Startup behaviour

MCP initialization is bounded, so a wedged server cannot blank the application
at launch. Servers that fail to start are reported in the sidebar with their
error, and Crush carries on. Servers that are live
[channels](/features/channels) are marked `channel` in the MCP list, so you can
confirm an opt-in actually took effect.

## Checking state

Ask the agent — the `crush_info` tool reports live MCP status along with the
active model, LSPs, skills, hooks, and permissions.

## Next

- **[Channels](/features/channels)** — MCP servers that push events at you.
- **[A2UI](/features/a2ui)** — rendering server-owned surfaces inline.
