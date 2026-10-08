---
id: configuration
title: Multi-agent configuration
sidebar_label: Configuration
description: Every setting that affects dispatched agents and todo enforcement, and the agent definitions that configure each agent.
---

# Multi-agent configuration

:::info[Fork feature]
Dispatched agents, todo enforcement and every key on this page exist only in
this fork.
:::

Beyond the `options.todo_enforcement` block, the tool deny list and
permissions, the `agents` block configures each agent: its model, prompt,
tools, MCP servers, skills and context files. See
[agent definitions](#agent-definitions). The runtime builds every agent from
its definition ([#432](https://github.com/joestump-agent/crush/issues/432)).
[Dispatched agents](/agents/overview) use the `worker` definition. Their
worktree location is still hard-coded.

## What you can set today

| Knob | `crushrc` | `crush.json` | Applies to |
| --- | --- | --- | --- |
| Todo nudges and hard gate | None ([#403](https://github.com/joestump-agent/crush/issues/403)) | `options.todo_enforcement.{enabled,nudge_threshold,hard_gate}` | Every agent except `agentic_fetch` |
| Kill thresholds | None ([#403](https://github.com/joestump-agent/crush/issues/403)) | `options.todo_enforcement.{kill_after_nudges,stall_window,hard_timeout}` | `kill_after_nudges`: every agent ([#393](https://github.com/joestump-agent/crush/issues/393)); the other two: dispatched agents |
| Turn dispatch off | `permissions deny dispatch_agent message_agent` | `options.disabled_tools` | The main agent |
| Dispatched-agent model | `agent set worker --model …` | `agents.worker.model` | The worker's slot (`small` by default) unless the main agent picks `large` or `small` on the call. A `{provider, model}` pin wins over both |
| Dispatched-agent tools | `agent set worker --tools …` | `agents.worker.tools` | The worker's allow list is the whole palette, minus your deny list |
| Skills a dispatch gets | `option skill-path`, `option disable-skill` | `options.skills_paths`, `options.disabled_skills` | Plus the per-call `skills` argument |
| Auto-approved tools | `permissions allow …` | `permissions.allowed_tools` | Read from the config the worktree loads (see [below](#config-the-dispatched-agent-reads)) |
| Yolo | `--yolo` at startup | — | Dispatched agents follow the startup flag. Turning yolo off with <kbd>ctrl+y</kbd> does not reach them ([#378](https://github.com/joestump-agent/crush/issues/378)) |
| A2A TCP listener | `option a2a-listen`, `a2a-tls-cert`, `a2a-tls-key`, `a2a-client-ca` | `options.a2a.{listen,tls_cert,tls_key,client_ca}` | The process's A2A host, beside its unix socket. TLS only: plain TCP and relative certificate paths fail the load. It exposes no local run yet. See [TCP listener](./a2a-protocol.md#tcp-listener) ([#358](https://github.com/joestump-agent/crush/issues/358)) |

## Todo enforcement settings

`options.todo_enforcement` is the global setting for every agent. How each
setting behaves is explained in
[Todo enforcement and wander kill](/agents/todo-enforcement).

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `enabled` | boolean | `true` | Inject nudges when an agent works without a todo list. `false` also turns off the `ignored nudges` kill |
| `nudge_threshold` | integer | `4` | Tool calls without todo activity before a nudge. A mutating call (`bash`, `edit`, `multiedit`, `write`) trips it regardless |
| `hard_gate` | boolean | `false` | Reject mutating tools until the session has a todo list. Works independently of `enabled` |
| `kill_after_nudges` | integer | `2` | Nudges a run may ignore before it is killed. `0` turns the kill off |
| `stall_window` | integer (seconds) | `0` | Kill a dispatched run whose todo list has not changed for this long. `0` is off |
| `hard_timeout` | integer (seconds) | `0` | Kill a dispatched run after this long, whatever its progress. `0` is off |

```json
{
  "$schema": "https://charm.land/crush.json",
  "options": {
    "todo_enforcement": {
      "enabled": true,
      "nudge_threshold": 8,
      "hard_gate": false,
      "kill_after_nudges": 0,
      "hard_timeout": 1800
    }
  }
}
```

### How values are read

Some values are not read the way they look:

- `nudge_threshold` of `0` or less means the default, `4`. It does not mean
  "nudge immediately"; use `1` for that.
- `kill_after_nudges` above `2` behaves exactly like `2`, because a run never
  gets more than two nudges. The schema's own example, `3`, is clamped.
- A negative `kill_after_nudges` silently means `0` (off).
- `stall_window` and `hard_timeout` are **whole seconds**. `0` or a negative
  value is off. A duration string such as `"30m"` fails to load.
- Unset keys take their defaults one at a time, so `{"hard_gate": true}` keeps
  the nudges and the kill on.

:::warning[Known issue]
These semantics are surprising and are tracked in [#401](https://github.com/joestump-agent/crush/issues/401). One more caveat on
current main: `stall_window` never fires ([#396](https://github.com/joestump-agent/crush/issues/396)).
:::

:::warning[Known issue]
There is no `crushrc` equivalent. `option todo-enforcement …` fails with
`option: unknown key`. Put the block in a `crush.json` next to your `crushrc`:
the two are deep-merged, and Crush logs a warning that both exist. Tracked in
[#403](https://github.com/joestump-agent/crush/issues/403).
:::

### Per-agent overrides

Agent definitions load and validate from both config formats
([#333](https://github.com/joestump-agent/crush/issues/333),
[#431](https://github.com/joestump-agent/crush/issues/431)), and each carries
its own `todos` and `kill` blocks. Dispatched agents resolve the worker
definition's blocks layered over the global `options.todo_enforcement`
([#402](https://github.com/joestump-agent/crush/issues/402)); the dispatch
tool's watchdog and the dispatched agent's ladder read the same resolved
settings, so a kill threshold configured on the definition trips in both
places. [#433](https://github.com/joestump-agent/crush/issues/433) lets
`dispatch_agent` choose which definition a dispatch runs.

`agents.<id>.todo_enforcement` stays accepted as a legacy alias for the
`todos` and `kill` blocks: its nudge knobs map onto `todos`, its kill knobs
onto `kill`. Setting the alias together with either block is a load error,
and on a non-dispatch agent its kill knobs are dropped with a warning (kill
is dispatch-only, the same rule the `kill` block enforces as a load error).

## What a dispatched agent can use

A dispatched agent gets the task agent's read-only tools plus five write tools.

| Group | Tools | Notes |
| --- | --- | --- |
| Always added | `bash`, `edit`, `multiedit`, `write`, `todos` | Added after your deny list is applied, so `permissions deny` cannot remove them ([#376](https://github.com/joestump-agent/crush/issues/376)) |
| Read-only set | `glob`, `grep`, `ls`, `view` | Your deny list can remove these |
| LSP lookups | `lsp_definition`, `lsp_symbols`, `lsp_call_hierarchy` | Only when an LSP is configured or `auto-lsp` is not turned off |
| Interactive parents only | `question` | Its questions pause the run and appear in your question prompt labeled with the agent's `@handle`; your deny list can remove it ([#352](https://github.com/joestump-agent/crush/issues/352)) |
| Never included | `agent`, `agentic_fetch`, `dispatch_agent`, `message_agent`, MCP tools, `semantic_search`, `sourcegraph` | Each would reach outside the worktree. Only one level of delegation is allowed |
| Missing today | `job_output`, `job_kill`, `lsp_diagnostics`, `fetch`, `download`, `lsp_references`, `lsp_rename`, `lsp_replace_symbol` | `bash` moves commands that run longer than 60 s into the background, but the agent has no `job_output` to read them back ([#384](https://github.com/joestump-agent/crush/issues/384)) |

Every path-based tool is rooted at the worktree. The agent's LSP servers start
in the worktree too and stop when the run ends.

**Model.** The main agent picks `"small"` (default) or `"large"` on each
`dispatch_agent` call. These resolve to your `models.small` and `models.large`
slots (`model small …` / `model large …` in `crushrc`). You cannot set a
default for dispatches or name a specific model.

## Permissions and yolo

A dispatched agent's permission requests show up in your normal permission
dialog, next to the main agent's. The request's path is the agent's worktree,
and its description carries the agent's `@handle`, though the dialog shows the
description only for some tools. Each request travels on the agent's A2A task
([#353](https://github.com/joestump-agent/crush/issues/353)): its run pauses
until you decide, other agents keep running, and requests from parallel tool
calls come one at a time.

- **Auto-approval** (`permissions.allowed_tools`) comes from your session's
  config, never from the worktree's. See
  [below](#config-the-dispatched-agent-reads).
- **Yolo** follows your live setting. Turning it on or off with
  <kbd>ctrl+y</kbd> applies to the next request of every running dispatched
  agent ([#378](https://github.com/joestump-agent/crush/issues/378)).
- **Bash allow-lists**: `--allow-commands`, `--allow-all-commands` and
  `options.allowed_commands` carry over from your session's config.
- **Kills**: if the agent is killed while its request waits on you, the request
  is denied and the agent's task ends canceled with the kill reason.

## Hooks

`PreToolUse` [hooks](/features/hooks#pretooluse) fire on the `dispatch_agent`
call itself, so a hook matching `^dispatch_agent$` can block or vet every
dispatch. They do **not** fire on the dispatched agent's own tool calls, even
though those include `bash` and `write` ([#377](https://github.com/joestump-agent/crush/issues/377)).

## Config the dispatched agent reads

A dispatched agent runs with your session's config, viewed from inside its
worktree. It never loads config from the worktree itself: nothing committed
on the base revision, a revision the main agent chooses, is read or executed
([#374](https://github.com/joestump-agent/crush/issues/374)).

- Policy (permissions, command allow-lists, LSP servers, skills, MCP) is your
  session's config: global plus the launch directory's project config,
  whether committed or not.
- Context files (`AGENTS.md`, `CRUSH.md`, …) and skills paths, resolved inside
  the worktree.
- The data directory is shared with your session, so logs and the database
  stay in one place.

## Hard-coded today

| Behaviour | Value |
| --- | --- |
| Worktree directory | `<launch directory>/.crush/worktrees/crush-dispatch-<id>`, ignoring `data_directory` ([#383](https://github.com/joestump-agent/crush/issues/383)) |
| Branch name | `crush-dispatch-<id>`, where `<id>` is a UUID |
| Base revision | The current branch (or `HEAD` when detached) unless the main agent passes `branch` |
| Concurrent dispatches | No limit |
| Diff in the result | Per-file stat plus the diff, cut at 250 lines |
| Nudges per run | 2 |
| Tool-loop detection | More than 5 identical tool calls and results in the last 10 steps |
| A2A server | One host per process on a unix socket, `<data dir>/a2a/<pid>.sock` (socket `0600`, directory `0700`), JSON-RPC, bearer-authenticated with a per-process token ([#346](https://github.com/joestump-agent/crush/issues/346), [#357](https://github.com/joestump-agent/crush/issues/357)). A TLS-only TCP listener is opt-in; see the table above |
| A2A client timeout | No total timeout; per-phase bounds only: 10 s dial, 10 s TLS handshake, 30 s response headers ([#344](https://github.com/joestump-agent/crush/issues/344)) |
| A2A server shutdown | 5 seconds |
| Cleanup when Crush exits | A sweep that force-removes every `crush-dispatch-*` worktree and branch, with a 30-second timeout. Whether it finishes depends on how Crush exits; see [troubleshooting](/agents/troubleshooting) |
| Inspect keys | <kbd>ctrl+]</kbd> and <kbd>ctrl+[</kbd> |

`apply_dispatch` and `dismiss_dispatch` record your decision about a
workspace explicitly ([#368](https://github.com/joestump-agent/crush/issues/368)). Exit and startup already keep decided and
work-producing workspaces and remove only the rest ([#367](https://github.com/joestump-agent/crush/issues/367)).

## Turning dispatch off

Deny both tools, and the main agent never sees them:

```bash
# crushrc
permissions deny dispatch_agent message_agent
```

```json
{
  "options": {
    "disabled_tools": ["dispatch_agent", "message_agent"]
  }
}
```

Sub-agents and dispatched agents never get these tools in any case.

## Agent definitions

:::info[Partially shipped]
Both config formats work: an `agents` block in `crush.json` and the `agent`
builtin in `crushrc` load, validate, and resolve to the same definitions
([#333](https://github.com/joestump-agent/crush/issues/333),
[#431](https://github.com/joestump-agent/crush/issues/431)). The runtime
honors `model`, `prompt`, `prompt_append`, `tools`, `mcp`, `skills`,
`context_paths` and `disabled`
([#432](https://github.com/joestump-agent/crush/issues/432)).

Still planned:

- the `agent` parameter on `dispatch_agent`, which picks a dispatch agent
  other than `worker`
  ([#433](https://github.com/joestump-agent/crush/issues/433));
- `runtime: a2a` agents behind an external card
  ([#434](https://github.com/joestump-agent/crush/issues/434));
- `workspace: none`.

Fields the runtime does not honor yet load with a one-time warning.
:::

The built-ins stay: `coder`, `plan`, `task`, and a new `worker` that replaces
today's hard-coded dispatch setup. You override a built-in field by field, or
add your own agents. A2A is the runtime contract: an agent runs in-process
(`builtin`) or behind an external Agent Card (`a2a`).

The rules:

- Entries are keyed by ID and overlay the built-ins field by field.
- `role` says which slot an agent can fill: `main`, `subagent` or `dispatch`.
- A `null` or missing field inherits. `"off"` disables. A negative value is a
  load error.
- Durations are strings (`"5m"`).
- Unknown tool names or agent IDs fail at load time, and the error names the
  offending path.
- Your deny list, hooks and yolo setting still apply on top of every agent.
  A definition cannot widen them.

```json
{
  "agents": {
    "$defaults": {
      "todos": { "nudge": true, "nudge_after_tool_calls": 4, "hard_gate": false },
      "kill":  { "after_ignored_nudges": "off", "stall": "off", "timeout": "off" }
    },
    "coder": {
      "role": "main",
      "model": "large",
      "prompt": "builtin:coder",
      "tools": { "deny": ["sourcegraph"] },
      "mcp": { "allow": ["*"] }
    },
    "task": {
      "role": "subagent",
      "model": "large",
      "tools": { "allow": ["@read"] },
      "todos": { "nudge": false }
    },
    "plan": { "disabled": false, "todos": { "nudge": false } },
    "worker": {
      "role": "dispatch",
      "runtime": "builtin",
      "model": { "provider": "openai", "model": "gpt-5-mini" },
      "prompt": "builtin:dispatch",
      "prompt_append": "file:.crush/prompts/worker.md",
      "tools": { "allow": ["@read", "@write", "job_output", "job_kill", "lsp_diagnostics"] },
      "mcp": { "allow": ["github:get_issue"] },
      "skills": ["go-patterns"],
      "workspace": "worktree",
      "kill": { "after_ignored_nudges": 2, "stall": "5m", "timeout": "30m" }
    },
    "reviewer": {
      "role": "dispatch",
      "runtime": "a2a",
      "card": "https://reviewer.example.net/.well-known/agent-card.json",
      "auth": { "type": "bearer", "token": "$REVIEWER_TOKEN" },
      "workspace": "none",
      "transport": { "protocol": "jsonrpc", "idle_timeout": "2m" },
      "permissions": { "artifacts": "review" }
    }
  },
  "options": {
    "dispatch": {
      "default_agent": "worker",
      "max_concurrent": 4
    }
  }
}
```

How the runtime applies each field:

- **`model`.**
  - A slot (`large` or `small`) picks that model for the agent.
  - A `{provider, model}` pin runs the agent on that model, with your small
    model kept for auxiliary work. Changing models in the TUI leaves pins
    and slots in place.
  - For dispatches, the call's `agent` argument names the definition to run —
    omitting it takes `options.dispatch.default_agent`. The call's `model`
    argument picks a slot, defaulting to the selected agent's slot; a pinned
    definition refuses the argument instead of ignoring it.
- **`prompt`.**
  - `builtin:<id>` is one of the embedded prompts.
  - `file:<path>` is a Go template, rendered with the same data as the
    built-ins. Relative paths resolve against the working directory, at load
    time and at runtime.
  - `prompt_append` is appended verbatim after the rendered prompt.
- **`tools`.**
  - The worker's allow list is the dispatch palette. No write tools are
    forced on, so a worker defined with `"@read"` is a read-only reviewer.
  - An enabled `coder`, `plan`, or dispatch agent whose definition resolves
    to no tools fails at load. Your deny lists are policy, not definition
    errors: they keep their runtime behavior.
  - Your `options.disabled_tools` and `permissions deny` still apply last.
  - Dispatch is refused only when that deny list removes all of `bash`,
    `edit`, `multiedit` and `write`.
- **`mcp`.**
  - A dispatch gets the MCP tools the worker's `mcp.allow` names; the
    built-in worker has none.
  - MCP servers are process-wide: they run in the parent's directory, not
    in the dispatch's worktree.
- **`skills`.** This list filters the agent's available skills. A dispatch's
  `skills` argument can narrow the worker's list, never widen it.
- **`context_paths`.** These replace `options.context_paths` for that agent.
- **`disabled`.**
  - A disabled `task` removes the `agent` tool.
  - A disabled `plan` cannot be switched to.
  - With no enabled dispatch agent, the main agent loses `dispatch_agent`,
    `message_agent`, `cancel_dispatch`, `apply_dispatch` and
    `dismiss_dispatch`.
  - `coder` cannot be disabled.

The same configuration in `crushrc`:

```bash
agent defaults --nudge-after 4 --kill-after-nudges off
agent set coder --deny-tool sourcegraph
agent set task --nudge false
agent add worker --role dispatch --model openai/gpt-5-mini \
  --prompt builtin:dispatch --prompt-append .crush/prompts/worker.md \
  --tools @read @write job_output job_kill lsp_diagnostics \
  --mcp github:get_issue --skill go-patterns --workspace worktree \
  --kill-after-nudges 2 --stall 5m --timeout 30m
agent add reviewer --role dispatch --runtime a2a \
  --card https://reviewer.example.net/.well-known/agent-card.json \
  --bearer '$REVIEWER_TOKEN' --workspace none --idle-timeout 2m
option dispatch-default-agent worker
option dispatch-max-concurrent 4
```

In this design, per-agent thresholds are the `todos` and `kill` blocks inside
each definition. `agents.<id>.todo_enforcement` is accepted as a legacy alias
for them ([#402](https://github.com/joestump-agent/crush/issues/402)), and
`options.todo_enforcement` stays as the global default.
