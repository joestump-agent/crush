---
id: configuration
title: Multi-agent configuration
sidebar_label: Configuration
description: Every setting that affects dispatched agents — the agent definitions behind coder, plan, task and worker, how to add your own, the dispatch options, todo enforcement, permissions, hooks, and external A2A agents.
---

# Multi-agent configuration

:::info[Fork feature]
Dispatched agents, agent definitions, todo enforcement and every key on this
page exist only in this fork.
:::

Every agent Crush runs is built from an **agent definition**: the built-in
`coder`, `plan`, `task` and `worker`, plus any you add. A definition fixes the
agent's model, system prompt, tools, MCP servers, skills, context files and
todo thresholds. You edit them with the `agent` builtin in `crushrc` or the
`agents` block in `crush.json`; both formats load, validate and resolve to the
same thing. A dispatch runs the definition the `dispatch_agent` call names,
or `options.dispatch.default_agent` — the `worker` — when it names none.

## What you can set

| Knob | `crushrc` | `crush.json` | Applies to |
| --- | --- | --- | --- |
| Which agent a dispatch runs by default | `option dispatch-default-agent <id>` | `options.dispatch.default_agent` | Every `dispatch_agent` call without an `agent` argument. Must name an enabled dispatch agent; default `worker` |
| How many dispatches run at once | `option dispatch-max-concurrent N` | `options.dispatch.max_concurrent` | Live dispatches plus ones still provisioning. Default **4**; a value below 1 fails the load |
| A dispatch agent's model | `agent set worker --model small\|large\|<provider>/<model>` | `agents.worker.model` | The agent's slot, unless the call picks `large` or `small`. A `{provider, model}` pin runs every dispatch on that model and refuses the call's `model` argument |
| A dispatch agent's tools | `agent set worker --tools …` / `--deny-tool …` | `agents.worker.tools.{allow,deny}` | The palette the agent is built with; your deny list still applies on top |
| MCP tools for a dispatch | `agent set worker --mcp …` | `agents.worker.mcp.allow` | Empty by default; `*`, a server id, or `server:tool` |
| Prompt and context | `--prompt`, `--prompt-append`, `--context-path`, `--skill` | `agents.<id>.{prompt,prompt_append,context_paths,skills}` | Any agent |
| Todo nudges and hard gate | `option todo-nudge`, `todo-nudge-threshold`, `todo-hard-gate`; `agent defaults` / `agent set <id>` with `--nudge`, `--nudge-after`, `--hard-gate` | `options.todo_enforcement.{enabled,nudge_threshold,hard_gate}`; `agents.$defaults.todos`, `agents.<id>.todos` | Every agent whose tools include `todos` |
| Kill thresholds | `option todo-kill-after-nudges`, `dispatch-stall`, `dispatch-timeout`; `agent set worker --kill-after-nudges`, `--stall`, `--timeout` | `options.todo_enforcement.{kill_after_nudges,stall_window,hard_timeout}`; `agents.<id>.kill` | Dispatched runs only. Other agents are nudged and never killed |
| A2A inactivity backstop | — (JSON only) | `options.todo_enforcement.inactivity_timeout` | A served dispatch that yields no events for this many seconds ends **failed**. Also the default idle timeout for [external agents](#external-agents) |
| Auto-approved tools | `permissions allow …` | `permissions.allowed_tools` | Read from your session's config, never the worktree's |
| Yolo | `--yolo` at startup, <kbd>ctrl+y</kbd> live | — | Dispatched agents follow your live setting |
| Turn dispatch off | `permissions deny dispatch_agent message_agent cancel_dispatch apply_dispatch dismiss_dispatch`, or `agent set worker --disabled true` | `options.disabled_tools`, or `agents.worker.disabled` | The main agent |
| A2A TCP listener | `option a2a-listen`, `a2a-tls-cert`, `a2a-tls-key`, `a2a-client-ca` | `options.a2a.{listen,tls_cert,tls_key,client_ca}` | The process's A2A host, beside its unix socket. TLS only; see [Working with other agents](/agents/team) |

## Agent definitions

### The built-ins

| Id | Role | Model | Prompt | Tools | Notes |
| --- | --- | --- | --- | --- | --- |
| `coder` | `main` | `large` | `builtin:coder` | Everything, MCP included | The main agent. Cannot be disabled |
| `plan` | `main` | `large` | `builtin:plan` | `agent`, `@read` minus `semantic_search`, plus `question` | Plan mode. Disabling it removes the mode switch |
| `task` | `subagent` | `large` | `builtin:task` | `@read` | The `agent` tool's sub-agent. Disabling it removes the `agent` tool |
| `worker` | `dispatch` | `small` | `builtin:dispatch` | `@read`, `@write`, `job_output`, `job_kill`, `lsp_diagnostics` | The default dispatch agent, in a `worktree`. Disabling it with no other dispatch agent removes the five dispatch tools |

### The rules

- Ids match `^[a-z][a-z0-9-]{0,31}$`. Anything else is a load error.
- Entries overlay the built-in with the same id field by field. A `null` or
  missing field inherits; a list replaces the inherited list rather than
  appending to it.
- `role` says which slot an agent fills: `main`, `subagent` or `dispatch`. A
  built-in's role cannot change, and a new agent must be a `dispatch` agent.
- `runtime` is `builtin` (in process) or `a2a` (an [external Agent
  Card](#external-agents)). `agent set` refuses `--role` and `--runtime`; use
  `agent add` to create an agent with them.
- `0` or `"off"` disables a numeric knob. A negative value is a load error.
  Durations take a Go duration (`"5m"`) or bare seconds.
- Unknown tool names, MCP servers, skills paths, prompt files and model pins
  fail at load time, and the error names the offending path
  (`agents.worker.tools.allow[2]: unknown tool "x"`).
- Your deny list, hooks and yolo setting apply on top of every definition. A
  definition can narrow your policy, never widen it.

### Tool groups

`tools.allow` and `tools.deny` take tool names, the two groups, or `*` (every
tool):

| Group | Members |
| --- | --- |
| `@read` | `glob`, `grep`, `ls`, `lsp_call_hierarchy`, `lsp_definition`, `lsp_symbols`, `semantic_search`, `sourcegraph`, `view` |
| `@write` | `bash`, `edit`, `multiedit`, `write`, `todos` |

Whether a named tool is actually registered still depends on the usual
conditions: the `lsp_*` tools need a language server (or `auto-lsp` on),
`semantic_search` an embeddings provider, and a dispatched agent is built
from a fixed list of worktree-rooted tools (see
[below](#what-a-dispatched-agent-can-use)).

### Fields

| Field | `crushrc` flag | Values |
| --- | --- | --- |
| `role` | `--role` | `main`, `subagent`, `dispatch` |
| `runtime` | `--runtime` | `builtin` (default) or `a2a` |
| `name`, `description` | `--name`, `--description` | Display values; the name is the agent's A2A card name |
| `disabled` | `--disabled` | `true` removes the agent (see [disabled](#how-fields-apply)) |
| `model` | `--model` | `"large"`, `"small"`, or `{ "provider": "…", "model": "…" }` (`provider/model` in crushrc) |
| `prompt` | `--prompt` | `builtin:<coder\|plan\|task\|dispatch>` or `file:<path>`; a bare path in crushrc means `file:` |
| `prompt_append` | `--prompt-append` | `file:<path>`, appended verbatim after the rendered prompt |
| `tools` | `--tools` (variadic), `--deny-tool` (repeatable) | `allow` and `deny` lists of tool names, `@read`, `@write`, `*` |
| `mcp` | `--mcp` (variadic) | `allow` list: `*`, a server id from `mcp`, or `server:tool` |
| `skills` | `--skill` (repeatable) | Skill names the agent may use |
| `context_paths` | `--context-path` (repeatable) | Context files for this agent, replacing `options.context_paths` |
| `workspace` | `--workspace` | `worktree` or `none`. Dispatch agents only; see [workspace](#how-fields-apply) |
| `todos` | `--nudge`, `--nudge-after`, `--hard-gate` | `nudge` (bool), `nudge_after_tool_calls` (int or `off`), `hard_gate` (bool) |
| `kill` | `--kill-after-nudges`, `--stall`, `--timeout` | `after_ignored_nudges` (int or `off`), `stall`, `timeout` (durations or `off`). Dispatch agents only |
| `card`, `auth`, `transport` | `--card`, `--bearer`, `--idle-timeout` | [External agents](#external-agents) only |
| `todo_enforcement` | — | Legacy alias for `todos` and `kill` with the `options.todo_enforcement` shape. Cannot be combined with either block |
| `$defaults` | `agent defaults` | Only `todos` and `kill`: the knobs every agent inherits |

### How fields apply

- **`model`.** A slot (`large` or `small`) picks that slot's model and follows
  it when you switch models in the TUI. A `{provider, model}` pin runs the
  agent on exactly that model (your small model is still used for auxiliary
  work), and the provider and model must exist in your config. For a
  dispatch, the call's `model` argument picks a slot — defaulting to the
  selected agent's — and is refused for a pinned definition.
- **`prompt`.** `builtin:<id>` is one of the embedded prompts. `file:<path>`
  is a Go template rendered with the same data as the built-ins (working
  directory, git status, skills, context files). Relative paths resolve
  against the working directory, at load time and at runtime, so a missing
  file is a load error. `prompt_append` is appended after the rendered
  prompt, verbatim.
- **`tools`.** The allow list is the agent's whole palette: no tool is forced
  on, so a dispatch agent allowed `@read` alone is a read-only reviewer, and
  an omitted allow list means every tool. An enabled `coder`, `plan` or
  dispatch agent whose definition resolves to no tools — an empty allow list,
  or a deny list that removes everything — fails the load. Your `permissions deny` list is applied last and is
  policy, not a definition error. A dispatch is refused only when the deny
  list removes all of `bash`, `edit`, `multiedit` and `write`.
- **`mcp`.** A dispatch gets the MCP tools its definition's `mcp.allow`
  names; the built-in `worker` names none. MCP servers are process-wide:
  they run in your session's directory, not in the dispatch's worktree.
- **`skills`.** Filters the skills the agent sees. A `dispatch_agent` call's
  `skills` argument can narrow the definition's list, never widen it, and a
  name that matches no discovered skill fails the call with
  `unknown skills: …`.
- **`context_paths`.** Replaces `options.context_paths` for that agent. A
  dispatched agent resolves them inside its worktree.
- **`workspace`.** A `runtime: a2a` agent always runs with `none`. A builtin
  dispatch agent always runs in a `worktree`: `none` is accepted but ignored
  for it.
- **`disabled`.** A disabled `task` removes the `agent` tool; a disabled
  `plan` cannot be switched to; with no enabled dispatch agent, the main agent
  loses `dispatch_agent`, `message_agent`, `cancel_dispatch`,
  `apply_dispatch` and `dismiss_dispatch`. `coder` cannot be disabled.
- **`todos`, `kill`.** Per-agent thresholds, layered over
  `options.todo_enforcement`. A `kill` block on a non-dispatch agent is a load
  error (`kill thresholds apply to "dispatch" agents only`). The semantics
  are on [Todo enforcement](/agents/todo-enforcement).

### Customizing the built-ins

Both blocks below load as written, in a project with no other configuration.
In `crushrc`:

```bash
agent defaults --nudge-after 4 --kill-after-nudges off
agent set coder --deny-tool sourcegraph
agent set task --nudge false
agent set plan --nudge false
agent set worker --model large \
  --tools @read @write job_output job_kill lsp_diagnostics lsp_references \
  --kill-after-nudges 2 --stall 5m --timeout 30m
option dispatch-max-concurrent 2
```

The same in `crush.json`:

```json
{
  "$schema": "https://charm.land/crush.json",
  "agents": {
    "$defaults": {
      "todos": { "nudge_after_tool_calls": 4 },
      "kill": { "after_ignored_nudges": "off" }
    },
    "coder": { "tools": { "deny": ["sourcegraph"] } },
    "task": { "todos": { "nudge": false } },
    "plan": { "todos": { "nudge": false } },
    "worker": {
      "model": "large",
      "tools": { "allow": ["@read", "@write", "job_output", "job_kill", "lsp_diagnostics", "lsp_references"] },
      "kill": { "after_ignored_nudges": 2, "stall": "5m", "timeout": "30m" }
    }
  },
  "options": {
    "dispatch": { "max_concurrent": 2 }
  }
}
```

To pin the worker to a specific model, name a provider and model that exist
in your config: `agent set worker --model openai/gpt-5-mini` in `crushrc`, or
`"model": { "provider": "openai", "model": "gpt-5-mini" }` in JSON. To give it
MCP tools, name a server from your `mcp` block:
`agent set worker --mcp github:get_issue`. Each of these fails the load, with
the path named, when the provider, model or server is not configured.

### Adding a dispatch agent

A worked example: a `tester` that the main agent can pick instead of the
`worker`, with its own prompt file. Write the prompt first, relative to the
directory you run Crush in:

```bash
mkdir -p .crush/prompts
cat > .crush/prompts/tester.md <<'MD'
You write tests and nothing else. Use the project's existing test framework,
run only the package you changed, and report the command you ran and its
result in your final message.
MD
```

Then define the agent. In `crushrc`:

```bash
agent add tester --role dispatch --model small \
  --name Tester --description "Writes tests for one package" \
  --tools @read @write job_output job_kill lsp_diagnostics \
  --prompt builtin:dispatch --prompt-append .crush/prompts/tester.md \
  --kill-after-nudges 2 --timeout 30m
```

In `crush.json`:

```json
{
  "$schema": "https://charm.land/crush.json",
  "agents": {
    "tester": {
      "role": "dispatch",
      "model": "small",
      "name": "Tester",
      "description": "Writes tests for one package",
      "tools": { "allow": ["@read", "@write", "job_output", "job_kill", "lsp_diagnostics"] },
      "prompt": "builtin:dispatch",
      "prompt_append": "file:.crush/prompts/tester.md",
      "kill": { "after_ignored_nudges": 2, "timeout": "30m" }
    }
  }
}
```

A new agent inherits nothing from `worker`, so set the three explicitly, as
above: without `--model` it resolves to `large`; without `--prompt` it runs
on the dispatch prompt anyway; and without `--tools` it is allowed **every**
tool name, which for a dispatch means the whole worktree palette — `fetch`,
`download` and `lsp_rename` included. An explicit empty allow list
(`"allow": []`) fails the load with `enabled agent resolves to no tools`.

Once it loads, the `dispatch_agent` tool's `agent` parameter lists `tester`
beside `worker`, and asking the main agent to "dispatch the tester agent to
cover pkg/x" produces `dispatch_agent {"agent": "tester", "prompt": …}`. The
result's `agent` field says which definition ran. To make `tester` the
default for every dispatch:

```bash
option dispatch-default-agent tester
```

A read-only reviewer is the same recipe with `--tools @read` — no write
tools means the agent can inspect the base revision and report, nothing
else. To remove an agent you added, `agent remove tester`; built-ins are
disabled with `agent set <id> --disabled true` instead.

### Load errors

Each error names the config path that caused it. The ones you are most
likely to meet:

| Message | Fix |
| --- | --- |
| `agents.<id>: id must match ^[a-z][a-z0-9-]{0,31}$` | Rename the agent |
| `agents.<id>.role: a new agent must be a "dispatch" agent.` | Only `dispatch` agents can be added |
| `agents.<id>.tools.allow[N]: unknown tool "x"` | Check the [tool reference](/reference/tools) |
| `agents.<id>.mcp.allow[N]: unknown MCP server "x"` | Define the server in your `mcp` block first |
| `agents.<id>.model: unknown provider "x"` / `provider "x" has no model "y"` | Pin only models your config knows |
| `agents.<id>: prompt file "…" does not exist` | The path is relative to the directory Crush runs in |
| `agents.<id>: enabled agent resolves to no tools.` | The allow list is empty, or `deny` removed everything; name at least one tool |
| `agents.<id>.kill: kill thresholds apply to "dispatch" agents only.` | `kill` belongs on `worker` or an agent you added |
| `agents.<id>.todo_enforcement: set either todo_enforcement or agents.<id>.todos, not both` | Drop the legacy alias |
| `agents.coder.disabled: the coder agent cannot be disabled.` | Deny tools instead |
| `options.dispatch.default_agent: unknown agent "x"` | Name an enabled dispatch agent |
| `option: dispatch-max-concurrent expects a positive integer` | At least 1 |
| `agent set: unknown agent "x" (define it first with agent add)` | `agent set` only overlays existing ids |
| `agent remove: "worker" is a built-in agent; disable it with …` | Built-ins are disabled, not removed |

A `crushrc` error is reported as the last line of a longer
`Failed to load config from paths […]` message; the `agents.…` path is the
part to read.

### Per-agent overrides

Each definition's `todos` and `kill` blocks layer over the global
`options.todo_enforcement`; the dispatch tool's watchdog and the dispatched
agent's ladder read the same resolved settings, so a threshold on the
definition trips in both places. `agents.<id>.todo_enforcement` stays
accepted as a legacy alias: its nudge knobs map onto `todos`, its kill knobs
onto `kill`. Setting the alias together with either block is a load error,
and on a non-dispatch agent its kill knobs are ignored (kill is
dispatch-only, the rule the `kill` block enforces as a load error).

## What a dispatched agent can use

A dispatched agent is built from a fixed set of worktree-rooted tools,
filtered by its definition's allow list, then by your deny list:

| Group | Tools | Notes |
| --- | --- | --- |
| In the `worker` palette | `glob`, `grep`, `ls`, `view`, `bash`, `edit`, `multiedit`, `write`, `todos`, `job_output`, `job_kill` | `bash` moves commands longer than 60 s to the background; `job_output` reads them back |
| With a language server (or `auto-lsp` on) | `lsp_definition`, `lsp_symbols`, `lsp_call_hierarchy`, `lsp_diagnostics` | The agent's LSP servers start in the worktree and stop when the run ends |
| Interactive parents only | `question` | Pauses the run and appears in your question prompt labeled with the `@handle`. Your deny list can remove it |
| Available if you add them to `tools.allow` | `fetch`, `download`, `lsp_references`, `lsp_rename`, `lsp_replace_symbol`, `lsp_restart`, `crush_logs` | Built, but not in the `worker`'s default list |
| MCP tools | Whatever `mcp.allow` names | None by default. The servers run in your session's directory |
| Never built for a dispatch | `agent`, `agentic_fetch`, `semantic_search`, `sourcegraph`, `dispatch_agent`, `message_agent`, `cancel_dispatch`, `apply_dispatch`, `dismiss_dispatch` | Each reaches outside the worktree or across the one-level delegation boundary. `semantic_search` and `sourcegraph` are in `@read` but are not constructed for a dispatch |

Every path-based tool refuses a path outside the worktree. `bash` is only
advised to stay inside; the dispatch prompt tells the agent to.

## Permissions and yolo

A dispatched agent's permission requests show up in your normal permission
dialog, next to the main agent's, whenever the TUI is up. The request's path is
the agent's worktree, and its description carries the agent's `@handle`. Each
request travels on the agent's A2A task: its run pauses until you decide,
other agents keep running, and requests from parallel tool calls come one at a
time.

- **Auto-approval** (`permissions.allowed_tools`) comes from your session's
  config, never from the worktree's. See
  [below](#config-the-dispatched-agent-reads).
- **Yolo** follows your live setting. Turning it on or off with
  <kbd>ctrl+y</kbd> applies to the next request of every running dispatched
  agent.
- **Bash allow-lists**: `--allow-commands`, `--allow-all-commands` and
  `options.allowed_commands` carry over from your session's config.
- **Kills**: if the agent is killed while its request waits on you, the request
  is denied and the agent's task ends canceled with the kill reason.

## Hooks

`PreToolUse` [hooks](/features/hooks#pretooluse) fire twice over: on the
`dispatch_agent` call itself, so a hook matching `^dispatch_agent$` can block
or vet every dispatch, and on the dispatched agent's own tool calls, which are
wrapped with the parent's hooks. The hook payload carries the dispatched
session's ID, and the hook commands run from your session's directory, never
from the worktree, so a policy that blocks `git push -f` or `rm -rf` stops a
dispatched agent too.

## Config the dispatched agent reads

A dispatched agent runs with your session's config, viewed from inside its
worktree. It never loads config from the worktree itself: nothing committed
on the base revision — a revision the main agent chooses — is read or
executed.

- Policy (permissions, command allow-lists, LSP servers, skills paths, MCP,
  hooks) is your session's config: global plus the launch directory's project
  config, whether committed or not.
- Context files (`AGENTS.md`, `CRUSH.md`, …) and skills are resolved inside
  the worktree.
- The data directory is shared with your session, so logs and the database
  stay in one place.

## Fixed behaviour

| Behaviour | Value |
| --- | --- |
| Worktree directory | `<data dir>/worktrees/<repo-key>/crush-dispatch-<id>`. The data directory is `crush dirs`'s (`.crush` in the project by default); the repo key is 12 hex characters derived from the repository's git directory, so every checkout of one repository shares it. `<data dir>/worktrees/.gitignore` is `*` |
| Branch name | `crush-dispatch-<id>`, where `<id>` is a UUID |
| Base revision | The current branch (or `HEAD` when detached) unless the main agent passes `branch` |
| Diff in the result | Per-file stat plus the diff, cut at 250 lines |
| Tool-loop detection | More than 5 identical tool calls and results in the last 10 steps |
| A2A host | One per process on a unix socket, `<data dir>/a2a/<pid>.sock` (socket `0600`, directory `0700`), JSON-RPC, bearer-authenticated with a per-process token. When that path would exceed the 104-byte socket limit, the socket is `$TMPDIR/crush-a2a-<uid>/<pid>-<hash>.sock` instead. A TLS-only TCP listener is opt-in |
| A2A client timeouts | No total timeout; per-phase bounds only: 10 s dial, 10 s TLS handshake, 30 s response headers |
| A2A server shutdown | 5 seconds per dispatch route |
| Cleanup when Crush exits | Every live dispatch is canceled with reason `crush exited`, then a 30-second sweep removes the workspaces you applied or dismissed and the ones with no work. Everything else stays |
| Inspect keys | <kbd>ctrl+]</kbd> in; <kbd>esc</kbd> or <kbd>ctrl+[</kbd> out; <kbd>ctrl+x</kbd> cancels |

## Todo enforcement settings

`options.todo_enforcement` is the global default for every agent, and the
`todos` and `kill` blocks of a definition override it per agent. The ladder
itself is explained on [Todo enforcement](/agents/todo-enforcement).

| Key | `crushrc` | Type | Default | Meaning |
| --- | --- | --- | --- | --- |
| `enabled` | `option todo-nudge` | boolean | `true` | Inject nudges when an agent works without a todo list |
| `nudge_threshold` | `option todo-nudge-threshold` | integer or `"off"` | `4` | Tool calls without todo activity before a nudge; a mutating call trips it at once. `0`/`off` disables nudging (same as `enabled: false`). The crushrc key takes at least 1 |
| `hard_gate` | `option todo-hard-gate` | boolean | `false` | Reject mutating tools until the session has a todo list |
| `kill_after_nudges` | `option todo-kill-after-nudges` | integer or `"off"` | `2` | Nudges a **dispatched** run may ignore before it is killed. Any positive value is honored; `0`/`off` turns the kill off |
| `stall_window` | `option dispatch-stall` | duration | `0` | Kill a dispatched run whose todo list has not changed for this long. Bare seconds, `"30m"`, or `"off"` |
| `hard_timeout` | `option dispatch-timeout` | duration | `0` | Kill a dispatched run after this long, whatever its progress. Same forms |
| `inactivity_timeout` | — | integer seconds | `0` | A2A backstop: a served run with no events at all for this long ends **failed**, with the reason. Also the default `transport.idle_timeout` for external agents |

Negative values fail the load. Unset keys take their defaults one at a time,
so `{"hard_gate": true}` keeps the nudges and the kill on.

```bash
# crushrc
option todo-nudge-threshold 8
option todo-kill-after-nudges off
option dispatch-timeout 30m
```

```json
{
  "$schema": "https://charm.land/crush.json",
  "options": {
    "todo_enforcement": {
      "nudge_threshold": 8,
      "kill_after_nudges": "off",
      "hard_timeout": "30m",
      "inactivity_timeout": 900
    }
  }
}
```

## External agents

A `runtime: a2a` definition points a dispatch agent at an
[A2A Agent Card](https://a2a-protocol.org) hosted somewhere else: a reviewer
your team runs, or a third-party agent. The main agent dispatches to it like
any other dispatch agent, with `dispatch_agent {agent: "reviewer"}`, and the
run goes through the same A2A client as the built-ins: the remote task's ID
is tracked, and a dropped stream is resumed. How this fits a team — and why
`--card` cannot point at another Crush yet — is on
[Working with other agents](/agents/team).

In `crushrc`:

```bash
agent add reviewer --role dispatch --runtime a2a \
  --card https://reviewer.example.net/.well-known/agent-card.json \
  --bearer '$REVIEWER_TOKEN' --workspace none \
  --idle-timeout 2m --timeout 30m
```

The same agent in `crush.json`:

```json
{
  "$schema": "https://charm.land/crush.json",
  "agents": {
    "reviewer": {
      "role": "dispatch",
      "runtime": "a2a",
      "card": "https://reviewer.example.net/.well-known/agent-card.json",
      "auth": { "type": "bearer", "token": "$REVIEWER_TOKEN" },
      "workspace": "none",
      "transport": { "idle_timeout": "2m" },
      "kill": { "timeout": "30m" }
    }
  }
}
```

Both load as written: the card is fetched when the agent is dispatched, not
when the config loads, and `$REVIEWER_TOKEN` is resolved per dispatch.

The fields:

| Field | Meaning |
| --- | --- |
| `card` | Required. The card's URL. It must be `https`; plain `http` is allowed only for `localhost` and loopback addresses, and the name must resolve to loopback when Crush dials it. A URL with no path, or `/`, fetches `/.well-known/agent-card.json`. Credentials in the URL are not allowed. |
| `auth` | Optional. `type` is `bearer`, the only type. `token` is required with it, and is usually a `$VAR` or `$(cmd)` reference. `--bearer` in `crushrc` accepts only a reference, never a literal token. A config layer that sets `card` drops the `auth` earlier layers gave the agent, so a project config that moves a card never inherits your global token; set `auth` again next to the new `card`. |
| `workspace` | `none`, the default and the only value. An external agent never touches your disk. |
| `transport.idle_timeout` | How long the agent's stream may stay silent before Crush cancels its task. Unset falls back to `options.todo_enforcement.inactivity_timeout`, then to **5 minutes**, so an external run is bounded by default. `off` disables it. |
| `kill.timeout` | The hard timeout for the whole run. Unset falls back to `options.todo_enforcement.hard_timeout`, which is off by default. It is the only `kill` field an external agent may set. |
| `name`, `description`, `disabled`, `role` | As for any agent. `role` must be `dispatch`. |

`model`, `prompt`, `tools`, `mcp`, `skills`, `todos` and the other local
fields are load errors on an external agent
(`a runtime a2a agent is defined by its card and may not set this field`);
so is `workspace: worktree`. The `dispatch_agent` call refuses `model`,
`skills` and `branch` for the same reason.

A definition with a missing or refused `card`, `auth` without a `token`, or a
negative `idle_timeout` does not fail the whole config. The agent drops out
of `dispatch_agent`'s agent list, and a call that names it anyway is refused
with `agent "<id>" cannot be dispatched: <the problem>`. Making such an agent
the `dispatch-default-agent` also loads, and then every dispatch without an
`agent` argument is refused the same way — check the refusal text, since the
load itself stays silent about it.

What happens on each dispatch:

1. **The token is resolved.** Once the call has a concurrency slot,
   `auth.token` goes through the same `$VAR` and `$(cmd)` resolver as the rest
   of your config, on every dispatch, so a rotated token is picked up. The
   resolution ends with the tool call. A reference that resolves to nothing,
   or a command that fails, fails the call (`auth.token did not resolve; the
   log has the reason`, or `resolved to an empty value`); the reason,
   including the command's stderr, goes to the log and not to the model.
2. **The card is fetched and checked.** The fetch carries no credentials, has
   10-second dial and 30-second total timeouts, caps the card at 1 MiB, and
   never follows a redirect to another origin (five same-origin redirects at
   most). Crush then needs a JSON-RPC interface on the card URL's own origin,
   on protocol version 1.x, and, when `auth` is set, an HTTP bearer security
   scheme. Anything else fails the call with an error that names the card and
   the kind of failure, never text the remote sent — see
   [the failure strings](#failures).
3. **The prompt is sent.** It goes out as a new task, with no context ID of
   Crush's own. A card that streams gets `SendStreamingMessage`, and its calls
   wait at most 30 seconds for response headers. A card that declares
   `streaming: false` gets a blocking `SendMessage` that answers only when the
   task ends, so the idle timeout bounds the whole run: raise it for a slow
   non-streaming agent. No worktree, branch, toolchain or local agent is
   created. The dispatch still gets a registry entry, a handle and a role, so
   its agent block and `@handle` show up as usual.
4. **The result is delivered.** The terminal result carries `source`, the card
   URL. Its first line tells the main agent that the content came from an
   external agent and is untrusted.

### After the run

- `cancel_dispatch` and <kbd>ctrl+x</kbd> work; the dispatch ends `killed`
  with `canceled by user`. A silent stream ends it with `idle timeout`, the
  hard timeout with `hard timeout`.
- `apply_dispatch` and `dismiss_dispatch` refuse an external dispatch:
  `dispatch <id> ran on an external agent: nothing was written to disk, so
  there is no workspace or branch to apply or dismiss`. There is nothing to
  decide; the findings are the whole result.
- Steering is not supported: a message to the agent's `@handle`, or
  `message_agent`, fails with `steering external agents is not supported
  yet`. A Crush dispatch folds a steer into its running turn and streams the
  reply back, but A2A gives a third-party agent no such contract. The same
  message would start a second task there, and nothing would read its
  reply. Cancel the agent and dispatch again with the new instructions
  instead.

### Failures

Every failure the model sees starts `dispatch to external agent "<id>"
failed:`; what follows is one of these, with the card URL filled in and
nothing the remote sent:

| Text | Meaning |
| --- | --- |
| `a2a: resolve agent card <url>: HTTP status 404` | The card URL answered with that status (`401`/`403`: the card itself needs a credential Crush does not send) |
| `… resolve agent card <url>: the host name did not resolve` / `connection refused` / `timed out` | Network |
| `… resolve agent card <url>: TLS certificate verification failed` | The server certificate does not chain to a CA your system trusts; see [Working with other agents](/agents/team#certificates) |
| `… resolve agent card <url>: the response is not a valid Agent Card` / `the card is larger than 1048576 bytes` / `refusing a cross-origin redirect` / `stopped after 5 redirects` | The card itself |
| `a2a: agent card <url> declares no HTTP bearer security scheme; refusing to send the configured bearer token` | You set `auth`, but the card has no `securitySchemes` entry of type `http`/`bearer`. Drop `auth`, or fix the card |
| `a2a: agent card <url> names its JSON-RPC service on another origin than <origin>; refusing it …` | The card's interface URL is on another host or port than the card. Fetch the card from the origin that serves the agent |
| `a2a: agent card <url> offers no JSON-RPC interface on a protocol version crush speaks (1.x)` / `offers no supported transport …` | The card offers only gRPC or HTTP+JSON, or an incompatible version |
| `the external agent asked for input; crush does not forward an external agent's input, permission, or auth requests, so its task was canceled. Re-dispatch with a self-contained prompt` | The remote paused on `input-required` or `auth-required` |
| `agent "<id>" cannot be dispatched: agents.<id>.card: …` | The definition is unusable (missing card, `http` to a non-loopback host, credentials in the URL, `auth` without a token) |
| `dispatch unavailable: the A2A host cannot reach external agents` | The process's A2A host could not start |

### Trust model

An external agent is someone else's code. Crush treats its card and its
output as data, and never lets it steer where your credentials go:

- **Credentials go to one origin.** The token is attached only to requests to
  the card URL's origin (scheme, host and port). Every request off that origin
  is refused before it is sent. That covers a card whose service URL points
  elsewhere, which is refused before any request carries the token, and a
  redirect to another host. The pin is by origin, not path: a card may name
  any path on its own host as its service, so agents hosted under different
  paths of one host can receive each other's tokens. Give each agent a host
  of its own when that matters.
- **The token is not echoed.** It is not logged, persisted, or put in the
  registry, the result, or an error. If the remote echoes it back, it is
  scrubbed to `[REDACTED]`. The scrub is an exact match, run after invisible
  characters are dropped and before any text is cut, and a prefix of four or
  more bytes left at a cut is scrubbed too. A remote that sends the token
  encoded, or a deliberate fragment of it, is not caught. Crush's own host
  token never rides an external call.
- **Output is untrusted text.** Findings, failure reasons, and stream errors
  that carry the remote's words are labeled `UNTRUSTED:`. They are stripped
  of control and invisible characters, such as terminal escapes, zero-width
  spaces, bidirectional overrides and the Unicode tag block, and capped at
  32 KiB. A response body is cut off at 16 MiB, one server-sent event at
  4 MiB, and the output is collected from at most 64 artifacts. Artifacts are
  read as text and never applied to disk. There is no diff, and the agent's
  self-reported usage is not added to your session's cost. A task ID that is
  not 256 or fewer printable characters ends the stream.
- **No questions, no permissions.** If the agent asks for input or
  authentication, Crush does not forward the request. It cancels the remote
  task and fails the dispatch, saying why. Write the prompt so the agent does
  not need to ask.
- **Kills end the stream; `tasks/cancel` is best effort.** Cancel, the hard
  timeout, the idle timeout and Crush exiting each end the stream, and the
  dispatch ends `killed` with the reason. Once the remote has named its task,
  Crush also sends `tasks/cancel` carrying the reason, bounded at 10 seconds,
  and logs a failure. A kill that lands before the remote names a task cuts
  the stream only: there is no task to cancel, and the remote may keep
  working.
- **After a crash.** A dispatch the dead process left running fails at the
  next start. An external one has no workspace to preserve, so it fails as
  external, without the card URL, which the durable record does not keep.
