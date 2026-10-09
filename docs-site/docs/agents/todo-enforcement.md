---
id: todo-enforcement
title: Todo enforcement and wander kill
sidebar_label: Todo enforcement
description: How Crush nudges agents that work without a todo list, the opt-in hard gate, and the wander kill that stops dispatched agents that ignore both.
---

# Todo enforcement and wander kill

:::info[Fork feature]
The todo enforcement ladder and wander kill exist only in this fork. Upstream
Crush adds a soft reminder that the todo list is empty, and tells the model it
may ignore it. Nothing upstream escalates.
:::

Every progress surface for an agent reads its session's todo list: the
current-todo line and `done/total` ratio on the
[agent block](/agents/handles-and-inspect), the status next to a handle in the
`@` completions, and the progress events on the [A2A stream](/agents/a2a-protocol).
An agent that never writes a todo list shows none of these. You see a spinner
and nothing else.

The ladder fixes that in three steps. It nudges first, can gate mutating tools
if you turn that on, and kills a **dispatched** run that keeps ignoring it. It
is on by default and configured under `options.todo_enforcement` (the
`option todo-*` and `option dispatch-*` keys in `crushrc`) and per agent in a
definition's `todos` and `kill` blocks. See the
[configuration reference](/agents/configuration#todo-enforcement-settings).

## Who it applies to

An agent gets the ladder only if its tool set includes `todos`: every rung
asks the model to call that tool, so an agent that cannot gets no nudges and
no gate. The kill rung is wired only for dispatched runs, because only a
dispatched run has a parent that can re-dispatch it; every other agent tops
out at the escalating nudge.

| Agent | Has `todos` | Nudges | Hard gate | Can be killed |
| --- | --- | --- | --- | --- |
| Coder (the main agent) | Yes | Yes | When on | **No** — nudged, never canceled |
| Plan (plan mode) | No | No | No | No |
| Task (the `agent` tool's sub-agent) | No | No | No | No |
| [Dispatched agents](/agents/overview) on the `worker` definition, or any dispatch agent whose tools include `todos` | Yes | Yes | When on | Yes, for every reason below |
| A dispatch agent you define without `todos` (`--tools @read`) | No | No | No | Only by the watchdog: `stalled todos` cannot fire, `hard timeout` and `tool loop` can |
| `agentic_fetch`, [external agents](/agents/configuration#external-agents) | No | No | No | External: `hard timeout` and `idle timeout` only |

Denying `todos` with `permissions deny todos` removes the ladder from every
agent.

## The ladder

The ladder counts tool calls in **windows**. A run is one turn: your prompt and
everything the agent does until it stops. A window *trips* when the run has no
todo activity and either of these is true:

- `nudge_threshold` tool calls (default **4**) have been made since the run
  started or since the last nudge, or
- any **mutating** tool call has been made since then. One call is enough.

Each trip moves the run one rung up, and the counters reset, so the next rung
needs a whole new window.

| Rung | When | What happens |
| --- | --- | --- |
| 0. Prompt mandate | Dispatched agents only, from the start | The dispatch system prompt tells the agent to record its plan with `todos` before its first mutating call |
| 1. Nudge | First trip | A nudge is added before the model's next step |
| 2. Escalating nudge | Each further trip, up to the cap | A sterner nudge that warns of escalation |
| 3. Kill (`ignored nudges`) | Dispatched runs only: the trip after `kill_after_nudges` nudges have been given | The run is canceled |

How many nudges a run gets depends on whether the kill is wired:

| Run | `kill_after_nudges` | Nudges before the ladder stops |
| --- | --- | --- |
| Dispatched | `2` (default) | Nudge, escalating nudge, then the kill on the third trip |
| Dispatched | `1` | Nudge, then the kill on the second trip. The escalating nudge is never sent |
| Dispatched | `N` ≥ 3 | `N` nudges — the first is the plain one, the rest escalate — then the kill. Values are honored as written, not clamped |
| Dispatched | `0` or `off` | Nudge, escalating nudge, then nothing more for the rest of the run |
| Main agent | any | Nudge, escalating nudge, then nothing more. `kill_after_nudges` has no effect |

With the defaults (`nudge_threshold: 4`, `kill_after_nudges: 2`), a
dispatched run with no todos is nudged after 4 tool calls, nudged again after
4 more, and killed after 4 more. A mutating call trips a window on its own,
so three file writes in a row without todos go through all three rungs.

### The nudge text

The nudges are fixed strings:

> You have made tool calls without recording a todo list. Call the todos tool
> now to write down your plan and keep it current as you work; your progress
> surfaces depend on it.

> You are still working without a todo list after being reminded. Call the
> todos tool immediately, before any further tool calls. Ignoring this again
> will escalate.

Each nudge is saved on the session as a **user-role message**. It appears in
the transcript like something you typed, and it stays in the history for later
turns.

## What counts as todo activity

The ladder stands down for the rest of the run as soon as either of these is
true:

- the agent calls the `todos` tool. The call counts the moment the model makes
  it, whatever the result;
- the session **already had a todo list** when the turn started. Any todos
  count, including a list where every item is completed.

Every other tool call counts toward the window: read-only calls, failed calls
and calls you denied.

### What counts as mutating

The same classification drives the nudge counter and the hard gate:

| Tool | Mutating? |
| --- | --- |
| `write`, `edit`, `multiedit`, `download`, `lsp_rename`, `lsp_replace_symbol` | Always |
| `bash` | Unless the command parses as read-only (`git status`, `ls`, `cat`, `grep` and the like). An unparseable command counts as mutating |
| MCP tools | Unless the server marks the tool with a read-only hint |
| Everything else (`view`, `glob`, `grep`, `ls`, `lsp_*` lookups, `fetch`, `question`, …) | Never |

## Hard gate

Off by default. With `hard_gate: true` (`option todo-hard-gate on`), the
mutating tools refuse to run until the session has a todo list. The model
gets this tool error:

```text
no todo list exists for this session; call the todos tool to record your plan before mutating anything (todo enforcement hard gate)
```

The gate reads the session on every call, so once the agent writes its todos,
its next mutating call goes through. The gate does not depend on `enabled`:
`enabled: false, hard_gate: true` gives you the gate with no nudges, and
therefore no `ignored nudges` kill, because that rung only exists inside the
nudging.

## Wander kill

A wander kill cancels a dispatched run deterministically and records why.
The kill travels as an A2A `tasks/cancel` carrying the reason, so the block
and the result show it within a few seconds, whatever the transport.

| Reason (`killed_reason`) | Trips when | Setting |
| --- | --- | --- |
| `ignored nudges` | The window after the last allowed nudge completes with no todo activity | `kill_after_nudges` |
| `stalled todos` | The todo list has not changed for `stall_window` while the run keeps going. The watchdog arms as soon as the run has a todo list | `stall_window` |
| `hard timeout` | The run has lasted `hard_timeout`, waiting on a question or a permission included | `hard_timeout` |
| `tool loop` | In the last 10 steps, the same tool call with the same input and output appears more than 5 times | Not configurable |
| `canceled by user` | You pressed <kbd>ctrl+x</kbd>, or the main agent called `cancel_dispatch` | — |
| `crush exited` | Crush shut down while the run was live | — |
| `idle timeout` | An external agent's stream stayed silent past `transport.idle_timeout` | External agents only |

`stall_window` and `hard_timeout` are durations — bare seconds, a string such
as `"30m"`, or `"off"` — and default to off. They are watchdogs that run
alongside a dispatched run and do not depend on `enabled`. A separate
A2A-level backstop, `inactivity_timeout` (seconds, JSON only, default off),
ends a served run that yields **no events at all** for that long; it ends the
run `failed` with the reason rather than `killed`, since it catches runs the
watchdogs cannot see.

### What the main agent receives

When a dispatched agent is killed, the main agent gets a hidden follow-up turn.
It says the workspace is preserved and asks the main agent to re-dispatch or
dismiss, followed by the result:

```json
{
  "dispatch_id": "7f3c…",
  "handle": "tester",
  "agent": "worker",
  "branch": "crush-dispatch-7f3c…",
  "workspace_path": "/home/you/project/.crush/worktrees/2d805a3d4cdf/crush-dispatch-7f3c…",
  "session_id": "…",
  "status": "killed",
  "key_findings": "<the agent's last assistant message>",
  "diff_summary": "<per-file stat, then the diff, cut at 250 lines>",
  "killed_reason": "ignored nudges"
}
```

The agent block shows a **Killed** line with the reason, then the findings and
the diff. The workspace and branch stay for salvage until you apply or
dismiss them.

## Examples

Each pair sets the same thing. The `crushrc` keys write
`options.todo_enforcement`; the two formats merge when both files exist.

Keep the nudges, turn the kill off:

```bash
option todo-kill-after-nudges off
```

```json
{
  "$schema": "https://charm.land/crush.json",
  "options": { "todo_enforcement": { "kill_after_nudges": "off" } }
}
```

Nudge less often during read-heavy work, and give a dispatched agent a hard
stop at half an hour:

```bash
option todo-nudge-threshold 8
option dispatch-timeout 30m
```

```json
{
  "$schema": "https://charm.land/crush.json",
  "options": { "todo_enforcement": { "nudge_threshold": 8, "hard_timeout": "30m" } }
}
```

Strict: no nudges, but mutating tools are blocked until a todo list exists:

```bash
option todo-nudge false
option todo-hard-gate on
```

```json
{
  "$schema": "https://charm.land/crush.json",
  "options": { "todo_enforcement": { "enabled": false, "hard_gate": true } }
}
```

Off entirely. With `enabled` and `hard_gate` both false, the ladder does not
run (the watchdogs still honor `stall_window` and `hard_timeout` if set):

```bash
option todo-nudge false
```

```json
{
  "$schema": "https://charm.land/crush.json",
  "options": { "todo_enforcement": { "enabled": false } }
}
```

Per agent, on the definition — a patient worker and a quiet task agent:

```bash
agent set worker --nudge-after 8 --kill-after-nudges 3 --stall 10m
agent set task --nudge false
```

```json
{
  "$schema": "https://charm.land/crush.json",
  "agents": {
    "worker": {
      "todos": { "nudge_after_tool_calls": 8 },
      "kill": { "after_ignored_nudges": 3, "stall": "10m" }
    },
    "task": { "todos": { "nudge": false } }
  }
}
```

The definition's `todos` and `kill` blocks layer over `options.todo_enforcement`
field by field; `agents.<id>.todo_enforcement` is accepted as a legacy alias
for the pair. See [Per-agent overrides](/agents/configuration#per-agent-overrides).

## Diagnosing a kill

A kill writes a warning to the [log](/reference/logging):

| Log line | Source |
| --- | --- |
| `Todo enforcement killed the run` with `reason` | The `ignored nudges` rung |
| `Dispatch run killed by watchdog` with `reason` | `stalled todos` or `hard timeout` |

```bash
crush logs --tail 500 | grep -i 'killed'
```

More symptoms are covered in
[Multi-agent troubleshooting](/agents/troubleshooting).
