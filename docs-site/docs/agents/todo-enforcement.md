---
id: todo-enforcement
title: Todo enforcement and wander kill
sidebar_label: Todo enforcement
description: How Crush nudges agents that work without a todo list, the opt-in hard gate, and the wander kill that stops agents that ignore both.
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
if you turn that on, and kills a run that keeps ignoring it. It is on by
default and configured under `options.todo_enforcement`. See the
[configuration reference](/agents/configuration#todo-enforcement-settings).

## Who it applies to

Crush builds the coder, plan and task agents with the ladder, and every
dispatched agent too. `agentic_fetch` is the only agent built without it.

| Agent | Has a `todos` tool | Nudges | Hard gate | Can be killed |
| --- | --- | --- | --- | --- |
| Coder (the main agent) | Yes | Yes | When on | Yes, for ignored nudges (see the known issue below) |
| Plan (plan mode) | **No** | Yes | No mutating tools | Yes, for ignored nudges |
| Task (the `agent` tool's sub-agent) | **No** | Yes | No mutating tools | Yes, for ignored nudges |
| [Dispatched agents](/agents/overview) | Yes | Yes | When on | Yes, for all four reasons |
| `agentic_fetch` | No | No | No | No |

:::warning[Known issue]
The kill is meant for dispatched agents only, but with the defaults it also
cancels the main agent, plan mode and `agent`-tool sub-agents. The turn ends as
if you had pressed <kbd>esc</kbd> twice, and any queued prompts are dropped.
Plan and task agents have no `todos` tool, so they can never satisfy a nudge:
any plan or sub-agent run that reaches about a dozen tool calls is canceled.

**Workaround:** turn the kill off with `"kill_after_nudges": 0` (see
[Examples](#examples)). Tracked in [#393](https://github.com/joestump-agent/crush/issues/393) (kill only dispatched agents) and
[#394](https://github.com/joestump-agent/crush/issues/394) (no nudges for agents without a `todos` tool).
:::

## The ladder

The ladder counts tool calls in **windows**. A run is one turn: your prompt and
everything the agent does until it stops. A window *trips* when the run has no
todo activity and either of these is true:

- `nudge_threshold` tool calls (default **4**) have been made since the run
  started or since the last nudge, or
- any **mutating** tool (`bash`, `edit`, `multiedit`, `write`) has been called
  since then. One call is enough.

Each trip moves the run one rung up, and the counters reset, so the next rung
needs a whole new window.

| Rung | When | What happens |
| --- | --- | --- |
| 0. Prompt mandate | Dispatched agents only, from the start | The dispatch system prompt tells the agent to record its plan with `todos` before its first mutating call |
| 1. Nudge | First trip | A nudge is added before the model's next step |
| 2. Escalating nudge | Second trip | A sterner nudge that warns of escalation |
| 3. Kill (`ignored nudges`) | Next trip once `kill_after_nudges` nudges have been given | The run is canceled |

A run gets at most **two** nudges. With the defaults (`nudge_threshold: 4`,
`kill_after_nudges: 2`), a run with no todos is nudged after 4 tool calls,
nudged again after 4 more, and killed after 4 more. A mutating call trips a
window on its own, so three `bash` calls in a row without todos go through
all three rungs.

`kill_after_nudges` changes where the ladder stops:

| `kill_after_nudges` | Behaviour |
| --- | --- |
| `2` (default) | Nudge, escalating nudge, kill |
| `1` | Nudge, then kill on the next trip. The escalating nudge is never sent |
| `0` | Nudge, escalating nudge, then no more action for the rest of the run |
| `3` or more | Same as `2`. Values above 2 are clamped ([#401](https://github.com/joestump-agent/crush/issues/401)) |

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

:::warning[Known issue]
Tools are classified by name only. A read-only `bash` call like `git status`
counts as mutating, so in a fresh session the first shell command draws a nudge
right away. Some tools that do change files (`lsp_rename`,
`lsp_replace_symbol`, `download`) and all MCP tools are not counted as
mutating, so they also get past the hard gate. Tracked in [#395](https://github.com/joestump-agent/crush/issues/395).
:::

## Hard gate

Off by default. With `"hard_gate": true`, the mutating tools refuse to run
until the session has a todo list. The model gets this tool error:

```text
no todo list exists for this session; call the todos tool to record your plan before mutating anything (todo enforcement hard gate)
```

The gate reads the session on every call, so once the agent writes its todos,
its next mutating call goes through. The gate does not depend on `enabled`.
`"enabled": false, "hard_gate": true` gives you the gate with no nudges. It
also gives you no ignored-nudges kill, because that rung only exists inside the
nudging.

## Wander kill

A wander kill cancels a run deterministically and records why. There are four
reasons:

| Reason (`killed_reason`) | Trips when | Agents | Setting |
| --- | --- | --- | --- |
| `ignored nudges` | The window after the last allowed nudge completes with no todo activity | Every agent with the ladder (should be dispatched only, [#393](https://github.com/joestump-agent/crush/issues/393)) | `kill_after_nudges` |
| `stalled todos` | The todo list has not changed for `stall_window` seconds while the run keeps going | Dispatched | `stall_window` |
| `hard timeout` | The run has lasted `hard_timeout` seconds | Dispatched | `hard_timeout` |
| `tool loop` | In the last 10 steps, the same tool call with the same input and output appears more than 5 times | Dispatched (other agents just stop) | Not configurable |

`stall_window` and `hard_timeout` are integer seconds and default to `0`
(off). They are watchdogs that run alongside a dispatched run and do not depend
on `enabled`.

:::warning[Known issue]
`stall_window` never fires today. The stall watchdog only arms if the session
already has todos when the run starts, and a dispatched agent's session always
starts empty. Tracked in [#396](https://github.com/joestump-agent/crush/issues/396).
:::

### What the main agent receives

When a dispatched agent is killed, the main agent gets a hidden follow-up turn.
It says the workspace is preserved and asks the main agent to re-dispatch or
dismiss, followed by the result:

```json
{
  "dispatch_id": "7f3c…",
  "branch": "crush-dispatch-7f3c…",
  "workspace_path": "/home/you/project/.crush/worktrees/crush-dispatch-7f3c…",
  "session_id": "…",
  "status": "killed",
  "key_findings": "<the agent's last assistant message>",
  "diff_summary": "<per-file stat, then the diff, cut at 250 lines>",
  "killed_reason": "ignored nudges"
}
```

The agent block shows a **Killed** line with the reason, then the findings and
the diff. When a non-dispatched agent is killed, there is no payload: its turn
is canceled.

:::warning[Known issue]
That payload only arrives on the direct in-process path. Dispatches normally run
over the [A2A transport](/agents/a2a-protocol), and there a kill produces no
final status. The block stays at *running* until the A2A client's 3-minute
timeout, then shows **failed** with a stream error. It has no `killed_reason`
and no salvaged diff, and a `tool loop` stop is reported as *completed*. Tracked
in [#342](https://github.com/joestump-agent/crush/issues/342) and [#343](https://github.com/joestump-agent/crush/issues/343).

:::

## Examples

Todo enforcement is **JSON-only** today: there is no `crushrc` option for it
([#403](https://github.com/joestump-agent/crush/issues/403)). `crushrc` and `crush.json` in the same folder are merged, so you can
keep a small `crush.json` just for this block. Crush logs a warning when both
exist.

Keep the nudges and turn off the kill. This is the recommended setting on
current main:

```json
{
  "$schema": "https://charm.land/crush.json",
  "options": {
    "todo_enforcement": { "kill_after_nudges": 0 }
  }
}
```

Nudge less often during read-heavy work:

```json
{
  "options": {
    "todo_enforcement": { "nudge_threshold": 8, "kill_after_nudges": 0 }
  }
}
```

Strict: no nudges, but mutating tools are blocked until a todo list exists:

```json
{
  "options": {
    "todo_enforcement": { "enabled": false, "hard_gate": true }
  }
}
```

Off entirely. With `enabled` and `hard_gate` both false, the ladder does not
run:

```json
{
  "options": {
    "todo_enforcement": { "enabled": false }
  }
}
```

## Diagnosing a kill

A kill writes a warning to the [log](/reference/logging):

| Log line | Source |
| --- | --- |
| `Todo enforcement killed the run` with `reason` | The `ignored nudges` rung, for any agent |
| `Dispatch run killed by watchdog` with `reason` | `stalled todos` or `hard timeout` |

```bash
crush logs --tail 500 | grep -i 'killed'
```

More symptoms are covered in
[Multi-agent troubleshooting](/agents/troubleshooting).

Thresholds can also be set per agent in the `todos` and `kill` blocks of its
definition, with `agents.<id>.todo_enforcement` accepted as a legacy alias and
`options.todo_enforcement` as the default
([#402](https://github.com/joestump-agent/crush/issues/402)); see
[Agent definitions](/agents/configuration#per-agent-overrides).

:::info[Planned]
These changes are decided but not built yet:

- The kill applies only to dispatched agents. Other agents are nudged but never
  canceled ([#393](https://github.com/joestump-agent/crush/issues/393)).
- Agents without a `todos` tool are not nudged ([#394](https://github.com/joestump-agent/crush/issues/394)).
- `crushrc` gets option keys for every setting on this page ([#403](https://github.com/joestump-agent/crush/issues/403)).
- Values mean what they say: no silent clamp at 2, negative numbers are load
  errors, and durations such as `"5m"` are accepted ([#401](https://github.com/joestump-agent/crush/issues/401)).
:::
