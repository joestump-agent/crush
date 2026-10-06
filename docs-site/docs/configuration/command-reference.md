---
id: command-reference
title: Config command reference
sidebar_label: Command reference
sidebar_position: 2
description: Every crushrc builtin and every flag — provider, model, mcp, lsp, hook, permissions, and option.
---

# Config command reference

These are the builtins available inside a [`crushrc`](/configuration/crushrc).
They read like CLI help because that is exactly what they are: entity commands
use `add` to create or update and `remove` (aliased `rm`) to delete. Booleans
accept `true/false/1/0/yes/no`, in any case.

```text
Available Commands:
  provider      Manage model providers
  model         Manage models and model selection
  mcp           Manage MCP servers
  lsp           Manage language servers
  hook          Manage hooks
  permissions   Configure tool permissions
  option        Configure general Crush behavior
```

## provider

```text
Usage:
  provider [command]

Available Commands:
  add       Add or update a provider
  remove    Remove a provider and its custom models
  rm        Alias for remove
```

### `provider add`

Add a provider, or update an existing provider with the same ID.

```text
Usage:
  provider add <id> [flags]

Flags:
      --name string                 display name
      --type string                 provider type (openai, openai-compat, anthropic, ollama, …)
      --api-key string              API key
      --base-url string             API base URL
      --disable bool                disable without removing
      --flat-rate bool              use flat-rate billing
      --discover-models bool        auto-discover and merge provider models
      --system-prompt-prefix string text prepended to the system prompt
      --extra-header key value      add an HTTP header (repeatable)
      --extra-body JSON             merge a JSON object into request bodies
      --provider-options JSON       merge a provider-specific JSON object
```

```bash
provider add deepseek \
  --type openai-compat \
  --base-url "https://api.deepseek.com/v1" \
  --api-key "${DEEPSEEK_API_KEY:?set DEEPSEEK_API_KEY}"
```

Headers whose value resolves to the empty string — an unset `$VAR`, a `$(...)`
that prints nothing, or a literal `""` — are **dropped** from the outgoing
request rather than sent as a bare `Header:`. That makes env-gated headers safe:

```bash
provider add openai --extra-header OpenAI-Organization "$OPENAI_ORG_ID"
```

If `OPENAI_ORG_ID` is unset, the header is simply not sent.

`--extra-body` is a non-expanding JSON passthrough. Put env-driven values in
`--extra-header`, `--api-key`, or `--base-url`, all of which do expand.

### `provider remove`

Removes the provider **and all custom models registered on it**.

```text
Usage:
  provider remove <id>
  provider rm <id>
```

## model

Model references use the `<provider>/<id>` form printed by `crush models`.

```text
Usage:
  model [command]

Available Commands:
  add       Register a custom model on an existing provider
  remove    Remove a custom model
  rm        Alias for remove
  large     Set or print the large model
  small     Set or print the small model
```

### `model add`

```text
Usage:
  model add <provider>/<id> [flags]

Flags:
      --name string                 display name
      --context-window int          context window in tokens
      --default-max-tokens int      default maximum output tokens
      --can-reason bool             model supports reasoning
      --supports-images bool        model accepts image input
      --price-input float           input price per 1M tokens
      --price-output float          output price per 1M tokens
      --price-cache-create float    cache-creation price per 1M tokens
      --price-cache-hit float       cache-hit price per 1M tokens
      --reasoning-effort string     low, medium, or high
```

### `model remove`

```text
Usage:
  model remove <provider>/<id>
  model rm <provider>/<id>
```

### `model large`, `model small`

Set the large (coding) or small (titles, summaries) model slot. With
no model argument, prints the current selection.

```text
Usage:
  model large [<provider>/<id>] [flags]
  model small [<provider>/<id>] [flags]

Flags:
      --think                       enable thinking mode
      --reasoning-effort string     low, medium, or high
      --max-tokens int              maximum output tokens
      --temperature float           sampling temperature
      --top-p float                 top-p sampling (0–1)
      --top-k int                   top-k sampling
      --frequency-penalty float     frequency penalty
      --presence-penalty float      presence penalty
      --provider-options JSON       merge a provider-specific JSON object
```

```bash
model large openai/gpt-4o --think
echo "coding with: $(model large)"   # prints: openai/gpt-4o
```

## mcp

See [MCP servers](/features/mcp) for what these do.

```text
Usage:
  mcp [command]

Available Commands:
  add       Add or update an MCP server
  remove    Remove an MCP server
  rm        Alias for remove
```

### `mcp add`

```text
Usage:
  mcp add <name> [flags]

Flags:
      --type string                 stdio, sse, or http (default "stdio")
      --command string              executable for stdio servers
      --args string                 command argument (repeatable)
      --env key value               environment variable (repeatable)
      --url string                  URL for HTTP/SSE servers
      --header key value            HTTP header (repeatable)
      --timeout int                 startup timeout in seconds
      --disabled bool               disable without removing
      --disabled-tools string       deny a server tool (repeatable)
      --enabled-tools string        allow only these server tools (repeatable)
      --oauth bool                  enable OAuth 2.1 flow (HTTP only)
      --oauth-client-id string      pre-registered OAuth client ID
      --oauth-client-secret string  pre-registered OAuth client secret
      --oauth-callback-port int     fixed localhost port for the OAuth callback
```

```bash
mcp add github --type http \
  --url "https://api.githubcopilot.com/mcp/" \
  --header Authorization "Bearer $GH_PAT"
```

As with providers, a header whose value resolves to the empty string is dropped.

### `mcp remove`

```text
Usage:
  mcp remove <name>
  mcp rm <name>
```

## agent

See [Multi-agent configuration](/agents/configuration) for what the
`agents` block does and how definitions resolve.

```text
Usage:
  agent [command]

Available Commands:
  defaults  Set the todos and kill defaults every agent inherits
  add       Define or overlay an agent
  set       Overlay an existing agent (no --role, no --runtime)
  remove    Remove a user-added agent
  rm        Alias for remove
```

### `agent defaults`

Only the todos and kill flags are allowed; this writes the `agents.$defaults`
block every definition inherits from.

```text
Usage:
  agent defaults [flags]

Flags:
      --nudge bool                  inject nudges when working without todos
      --nudge-after int|off         tool calls before the first nudge
      --hard-gate bool              reject mutating tools until todos exist
      --kill-after-nudges int|off   ignored nudges before the wander kill
      --stall duration|off          kill a run whose todos stop updating
      --timeout duration|off        kill a run after this long
```

```bash
agent defaults --nudge-after 4 --kill-after-nudges off
```

### `agent add`

Defines a new agent or overlays an existing one (built-in or added earlier in
the same `crushrc`), field by field.

```text
Usage:
  agent add <id> [flags]

Flags:
      --role string              main, subagent, or dispatch
      --runtime string           builtin, or a2a for an external Agent Card
      --name string              display name
      --description string       agent description
      --disabled bool            disable without removing
      --model string             large, small, or provider/model
      --prompt string            builtin:<id>, file:<path>, or a bare path
      --prompt-append string     a prompt file appended to the system prompt
      --workspace string         dispatch isolation: worktree or none
      --card string              external Agent Card URL (runtime a2a)
      --tools string             tool allow list (variadic: entries until the
                                 next flag), each a tool name, @read, @write, or *
      --deny-tool string         tool to deny (repeatable)
      --mcp string               MCP allow list (variadic), each *, server, or server:tool
      --skill string             skill name (repeatable)
      --context-path string      context file path (repeatable)
      --nudge bool               see agent defaults
      --nudge-after int|off      see agent defaults
      --hard-gate bool           see agent defaults
      --kill-after-nudges int|off  see agent defaults
      --stall duration|off       see agent defaults
      --timeout duration|off     see agent defaults
      --bearer '$VAR'            bearer auth for runtime a2a agents; the value
                                 must be a $VAR reference, so no secret is
                                 ever written to the config
      --idle-timeout duration    end a silent runtime a2a run after this long
```

```bash
agent add worker --role dispatch --model openai/gpt-5-mini \
  --prompt builtin:dispatch --prompt-append .crush/prompts/worker.md \
  --tools @read @write job_output job_kill lsp_diagnostics \
  --kill-after-nudges 2 --stall 5m --timeout 30m
agent add reviewer --role dispatch --runtime a2a \
  --card https://reviewer.example.net/.well-known/agent-card.json \
  --bearer '$REVIEWER_TOKEN' --workspace none --idle-timeout 2m
```

Counts take a non-negative integer or `off`; durations take a Go duration
(`5m`), a bare number of seconds, or `off`. Unknown tools, role changes on
built-ins, and other semantic errors are reported when the config loads, not
here.

### `agent set`

```text
Usage:
  agent set <id> [flags]
```

The same flags as `agent add`, except `--role` and `--runtime`: those change
what an existing agent is, and `set` only overlays fields. The id must be a
built-in or already defined.

### `agent remove`

```text
Usage:
  agent remove <id>
  agent rm <id>
```

Removes a user-added agent. Built-in agents cannot be removed; disable one
instead:

```bash
agent remove scribe
agent set plan --disabled true
```

## lsp

See [Language servers](/features/lsp).

```text
Usage:
  lsp [command]

Available Commands:
  add       Add or update a language server
  remove    Remove a language server
  rm        Alias for remove
```

### `lsp add`

```text
Usage:
  lsp add <name> --command <command> [flags]

Flags:
      --args string              command argument (repeatable)
      --env key value            environment variable (repeatable)
      --filetypes string         file type to attach to (repeatable)
      --root-markers string      root marker file (repeatable)
      --timeout int              startup timeout in seconds
      --disabled bool            disable without removing
      --init-options JSON        initialization options
      --options JSON             server settings
```

```bash
lsp add go --command gopls --env GOPATH "$HOME/go"
```

### `lsp remove`

```text
Usage:
  lsp remove <name>
  lsp rm <name>
```

## hook

See [Hooks](/features/hooks).

```text
Usage:
  hook [command]

Available Commands:
  add       Add a hook to an event
  remove    Remove a named hook, or clear an event
  rm        Alias for remove
```

### `hook add`

```text
Usage:
  hook add <event> --command <command> [flags]

Flags:
      --command string           shell command to run (required)
      --name string              name used for later removal
      --matcher string           regex tested against the tool name
      --timeout int              timeout in seconds (default 30)
```

```bash
hook add PreToolUse --matcher "^bash$" \
  --command "./hooks/no-haskell.sh" --name no-haskell
```

### `hook remove`

Without `--name`, removes **every** hook for the event.

```text
Usage:
  hook remove <event> [--name <name>]
  hook rm <event> [--name <name>]

Flags:
      --name string              remove hooks with this name
```

## permissions

`allow` skips approval prompts; `deny` hides tools from the agent entirely. See
[Permissions](/configuration/permissions).

```text
Usage:
  permissions [command]

Available Commands:
  allow     Allow tools without prompting
  deny      Hide tools from the agent
```

```bash
permissions allow view ls grep edit
permissions deny bash sourcegraph
```

## option

```text
Usage:
  option <key> [value]
  option [command]

Available Commands:
  reset     Clear every value from a list option
  ui        Configure terminal UI behavior
```

Boolean values are optional and default to `true`, so `option debug` is the same
as `option debug true`.

### Boolean keys

| Key | Effect |
| --- | --- |
| `debug` | Enable debug logging |
| `debug-lsp` | Enable LSP debug logging |
| `auto-lsp` | Automatically configure language servers |
| `progress` | Show progress indicators |
| `metrics` | Send anonymous usage metrics |
| `auto-summarize` | Automatically summarise long conversations |
| `provider-auto-update` | Update the provider catalog automatically |
| `default-providers` | Include built-in providers |
| `attribution-generated-with` | Add the `Generated with Crush` line |

### String keys

| Key | Effect |
| --- | --- |
| `data-directory` | Directory for project data and state |
| `initialize-as` | Context filename created by `crush init` (default `AGENTS.md`) |
| `notifications` | `auto`, `native`, `osc`, `bell`, or `disabled` |
| `attribution-trailer-style` | `none`, `co-authored-by`, or `assisted-by` |

### List keys

Repeat the command to append multiple values.

| Key | Effect |
| --- | --- |
| `context-path` | Append a project context path |
| `global-context-path` | Append a global context path |
| `skill-path` | Append a skill directory |
| `disable-skill` | Hide a skill from the agent |

```bash
option progress false
option skill-path ./skills
option attribution-trailer-style assisted-by
option disable-skill crush-config
```

### Todo enforcement and dispatch keys

These keys configure the proactive todo enforcement ladder and the
dispatched-run timeouts, which live under `options.todo_enforcement` in the
JSON form. Durations accept a Go duration (`5m`) or a bare number of
seconds; counts and durations accept `off` for zero. A negative value or an
unparseable one fails the load, naming the key.

| Key | Effect |
| --- | --- |
| `todo-nudge true\|false` | Inject a nudge when an agent works without a todo list |
| `todo-nudge-threshold N` | Tool calls without todo activity before the first nudge (at least 1) |
| `todo-hard-gate on\|off` | Reject mutating tools until a todo list exists |
| `todo-kill-after-nudges N\|off` | Nudges a dispatched agent may ignore before it is killed |
| `dispatch-stall DUR\|off` | Kill a dispatched run whose todos stop updating for this long |
| `dispatch-timeout DUR\|off` | Kill a dispatched run after this long, whatever its progress |

```bash
option todo-nudge false
option todo-kill-after-nudges 3
option dispatch-stall 5m
option dispatch-timeout 1h
```

### `option reset`

Clears every value previously added to a list option. Values added *after* the
reset are kept — which is what makes `source`-ing a shared base config
overridable.

```text
Usage:
  option reset <key>

Available Keys:
  context-path          clear project context paths
  global-context-path   clear global context paths
  skill-path            clear additional skill directories
  disable-skill         clear disabled skill names
```

:::important
These skill paths load by default — you do **not** need `skill-path` for them:
`.agents/skills`, `.crush/skills`, `.claude/skills`, `.cursor/skills`.
:::

### `option ui`

```text
Usage:
  option ui <key> <value>

Available Keys:
  compact bool                  use the compact chat layout
  diff unified|split            choose unified or side-by-side diffs
  transparent bool              use the terminal background
  scrollbar string              chat scrollbar visibility: default, always, never
  completions-max-depth int     maximum directory depth shown by completions
  completions-max-items int     maximum items returned to completions
```

```bash
option ui compact true
option ui diff unified
option ui transparent true
option ui scrollbar always
option ui completions-max-depth 4
option ui completions-max-items 200
```
