List the prompts exposed by a connected MCP server.

MCP servers can expose reusable, server-authored prompt templates (distinct
from tools and resources). Use this tool to discover which prompts a server
offers and what arguments each one takes, then invoke one with `call_mcp_prompt`.

Parameters:
- `mcp_name` (required): the name of the MCP server, as configured in Crush.

The result lists each prompt's name and description, along with its arguments
(name, whether it is required, and a description). Argument names are what you
pass to `call_mcp_prompt`.

Notes:
- If the server exposes no prompts (or does not support prompts), this returns
  "No prompts found".
- This does not invoke anything; it only lists metadata. Use `call_mcp_prompt`
  to render a prompt's content.
