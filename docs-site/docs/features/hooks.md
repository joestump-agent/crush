---
id: hooks
title: Hooks
sidebar_position: 7
description: Shell commands that gate, rewrite, or annotate tool calls before they run — the execution model, the envelope, and the exit codes.
---

# Hooks

Hooks are user-defined shell commands that run when events happen during the
agent lifecycle. They give you **deterministic** control over the agent's
behaviour, in front of the permission flow rather than behind it.

Hot hook facts:

- Hooks are just shell commands.
- They can be written in any language, because they are just executables —
  Bash, Python, Node, Rust, Haskell, whatever.
- They are Claude Code-compatible.
- There is currently one event, `PreToolUse`, with more planned.
- They run in parallel for speed, but compose in config order for determinism.
- Crush ships a built-in `crush-hooks` skill, so you can just tell Crush what
  you want a hook to do.

Things people actually use them for:

- Block dangerous commands — no more `git push -f` or `rm -rf`
- Rewrite tool input — turn `node` into `deno`, scrub secrets from commands
- Inject context — "remember to run `gofumpt` after editing Go files"
- Auto-approve tools — skip the prompt for `bash` commands you know are safe
- Log certain tool calls

## Baby's first hook

Add this to your **project-level** `crush.json`:

```jsonc
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "^bash$",
        "command": "./no-haskell.sh"
      }
    ]
  }
}
```

…and write the script:

```bash
#!/usr/bin/env bash

# Disallow ghc, cabal, and stack.
if echo "$CRUSH_TOOL_INPUT_COMMAND" | grep -qE '(^| )((ghc|cabal|stack)(\.exe)?)( |$)'; then
  # Feedback goes to stderr; exit 2 blocks the call.
  echo "No Haskell allowed, kiddo." >&2
  exit 2
fi
```

Or in `crushrc`:

```bash
hook add PreToolUse --matcher "^bash$" \
  --command "./no-haskell.sh" --name no-haskell
```

:::important[Paths are resolved against the working directory]
`command` is resolved relative to your **current working directory**, not to the
config file. Relative paths work in a project-level config because the project
root *is* the working directory. In a global config
(`~/.config/crush/crush.json`) use an absolute path, or an inline command.
:::

## Configuration

| Field | Meaning |
| --- | --- |
| `command` | **Required.** The shell command to run |
| `matcher` | Regex tested against the tool name. Omit to match every tool |
| `name` | Friendly name shown in the TUI, and the handle for `hook remove --name` |
| `timeout` | Seconds; default 30 |

Hooks can be configured globally and per project; project-level hooks take
precedence.

## Events

### `PreToolUse`

Fires before every tool call. Matched against the **tool name** — `bash`,
`edit`, `write`, `mcp_github_create_pull_request`.

Event names are case-insensitive and snake-caseable: `PreToolUse`,
`pretooluse`, `PRETOOLUSE`, `pre_tool_use`, and `PRE_TOOL_USE` all work.

**Scope.** `PreToolUse` only fires on the **top-level agent's** tool calls.
Sub-agents (the `agent` task tool, `agentic_fetch`) run without hook
interception, so one delegated turn doesn't trigger your hook N times. The outer
sub-agent tool call itself *is* hooked, so a policy like "never let the agent
spawn sub-agents" still works.

## Execution model

Hooks run through Crush's embedded POSIX shell
([`mvdan.cc/sh`](https://mvdan.cc/sh)) — the same interpreter the `bash` tool
uses. Inline commands and shebang-less scripts execute in-process; scripts with
a `#!` shebang dispatch to the named interpreter via `os/exec`. The contract is
identical on macOS, Linux, and Windows.

In practice:

- **Windows without Unix tooling.** Inline shell (`echo`, pipelines, `jq`,
  `grep`), shebang-less `.sh` scripts, inline PowerShell
  (`powershell -Command …`), and `.exe` invocations all work with no WSL, Git
  Bash, Cygwin, or MSYS.
- **PowerShell scripts** (`.ps1`) are not auto-dispatched by extension. Invoke
  them explicitly: `powershell -File ./audit.ps1`.
- **Shebang'd scripts** need the interpreter on `PATH`. CRLF line endings in
  the shebang are tolerated. If the absolute path in a shebang doesn't exist
  (`#!/bin/bash` on Windows), Crush falls back to a `PATH` lookup of the base
  name before giving up; a debug log records the fallback. If it still isn't
  found, the hook fails cleanly as a non-blocking warning.
- **Environment.** Every hook sees `CRUSH=1`, `AGENT=crush`, and
  `AI_AGENT=crush` on top of the `CRUSH_*` variables. Those three markers match
  what the `bash` tool sets, so "am I being run by an AI agent?" detection
  behaves identically in both contexts.

When a hook fires, Crush:

1. Filters hooks whose `matcher` matches the tool name (no matcher = match all).
2. Deduplicates by `command` — identical commands run once.
3. Runs all matching hooks **in parallel**.
4. Waits for all of them, then aggregates results **in config order**.
5. Applies the result **before** the permission check.

## Input

### Environment variables

| Variable | Description |
| --- | --- |
| `CRUSH` | Always `1` under Crush |
| `AGENT` | Always `crush` |
| `AI_AGENT` | Always `crush` |
| `CRUSH_EVENT` | The hook event name, e.g. `PreToolUse` |
| `CRUSH_TOOL_NAME` | The tool being called, e.g. `bash` |
| `CRUSH_SESSION_ID` | Current session ID |
| `CRUSH_CWD` | Working directory |
| `CRUSH_PROJECT_DIR` | Project root directory |
| `CRUSH_TOOL_INPUT_COMMAND` | For `bash` calls: the shell command |
| `CRUSH_TOOL_INPUT_FILE_PATH` | For file tools: the target path |

### Stdin JSON

```jsonc
{
  "event": "PreToolUse",
  "session_id": "313909e",
  "cwd": "/home/user/project",
  "tool_name": "bash",
  "tool_input": { "command": "rm -rf /" }
}
```

`tool_input` is the raw JSON the model sent to the tool.

```bash
#!/usr/bin/env bash
read -r input
tool_name=$(echo "$input" | jq -r '.tool_name')
command=$(echo "$input" | jq -r '.tool_input.command // empty')
```

Remember that [`jq` is built in](/features/skills#jq), so this works with no
external binary.

## Output

### Exit codes

| Exit code | Meaning |
| --- | --- |
| `0` | Success. Stdout is parsed as JSON (see below). |
| `2` | **Block the tool.** Stderr is the deny reason. No JSON. |
| `49` | **Halt the turn.** Stderr is the halt reason. No JSON. |
| Other | Non-blocking error. Logged and ignored; the call proceeds. |

The difference between 2 and 49: **exit 2** blocks the current tool call — the
agent sees the error and can try something else. **Exit 49** halts the whole
turn; the agent doesn't get to respond further and you take over. Use 49 when
something is wrong enough that the agent shouldn't keep trying.

49 sits in an empty slice of the exit-code space — between the generic-error
range (1–30), the BSD `sysexits.h` range (64–78), and the killed-by-signal range
(128+) — so it cannot be hit by accident.

### The JSON envelope

For more control — rewriting input, injecting context — exit 0 and print JSON:

```jsonc
{
  "version": 1,                          // Optional; defaults to 1.
  "decision": "allow",                   // "allow", "deny", or null (no opinion).
  "halt": false,                         // If true, halts the turn entirely.
  "reason": "LGTM",                      // Shown when denying or halting.
  "context": "Scrubbed secrets",         // String or array; appended to what the model sees.
  "updated_input": { "command": "…" }    // Shallow-merged into the tool input.
}
```

**`decision: "allow"` is affirmative.** It pre-approves the call and bypasses
the permission prompt. Silence — no `decision`, or `null` — means "no opinion",
and the call goes through the normal permission flow. Omit it when you only want
to inject context or rewrite input without also vouching for the call.

**`updated_input` is a shallow-merge patch.** Keys you include overwrite
matching keys; keys you don't are preserved. If the model called `bash` with
`{"command": "npm test", "timeout": 60000}` and your hook returns
`{"updated_input": {"command": "bun test"}}`, the tool runs with
`{"command": "bun test", "timeout": 60000}` — the timeout isn't dropped. Nested
objects are replaced wholesale, not deep-merged.

**`context`** accepts a string or an array of strings. Empty strings and empty
entries are dropped.

```bash
#!/usr/bin/env bash
# Rewrite a bash command through a secret scrubber.

read -r input
original_cmd=$(echo "$input" | jq -r '.tool_input.command')
rewritten=$(secret-scrubber rewrite "$original_cmd")

cat <<EOF
{
  "decision": "allow",
  "context": "Scrubbed secrets",
  "updated_input": {"command": "$rewritten"}
}
EOF
```

## Aggregation

Hooks run in parallel but compose deterministically in **config order** —
finishing first doesn't let a hook win.

- If **any** hook denies, the call is blocked. `reason` values concatenate in
  config order, newline-separated.
- If **any** hook halts, the turn ends after the call is blocked.
- If none denies or halts but at least one allows, the call proceeds **and the
  permission prompt is skipped**.
- `context` values concatenate in config order. Strings and arrays compose
  uniformly.
- `updated_input` patches shallow-merge in config order; later hooks override
  earlier ones on colliding keys. Ignored entirely if denied or halted.

## Timeouts

Default 30 seconds. On timeout Crush cancels the context and treats the result
as a non-blocking error, so the tool call proceeds. Shebang-dispatched
subprocesses are killed through `exec.CommandContext`; in-process hooks get a
short grace period (~1s) to yield and are then abandoned, with a warning logged.
Long-running work should honour context cancellation or run out-of-process via a
shebang.

## Examples

### Block destructive commands

```bash
#!/usr/bin/env bash
# Block rm -rf. Otherwise stay silent so the normal permission flow runs.
if echo "$CRUSH_TOOL_INPUT_COMMAND" | grep -qE '\brm\s+(-[a-zA-Z]*r[a-zA-Z]*f|-[a-zA-Z]*f[a-zA-Z]*r)\b'; then
  echo "rm -rf is blocked by policy." >&2
  exit 2
fi
```

### Auto-approve read-only bash

```bash
#!/usr/bin/env bash
# Auto-approve read-only bash commands; stay silent on everything else.
case "$CRUSH_TOOL_INPUT_COMMAND" in
  ls\ *|cat\ *|git\ status*|git\ diff*|rg\ *)
    echo '{"decision":"allow","reason":"read-only"}'
    ;;
esac
```

### Inject context on Go edits

```bash
#!/usr/bin/env bash
# Emit context only; no decision, so the normal permission prompt still runs.
case "$CRUSH_TOOL_INPUT_FILE_PATH" in
  *.go) echo '{"context":"Run gofumpt after editing Go files."}' ;;
esac
```

### Block all MCP tools

```jsonc
{
  "hooks": {
    "PreToolUse": [
      { "matcher": "^mcp_", "command": "echo 'MCP is disabled here.' >&2; exit 2" }
    ]
  }
}
```

### Other languages

Any executable works. Lua, JavaScript, Python — pick whatever reads stdin and
writes stdout.

```javascript
#!/usr/bin/env node
const input = JSON.parse(require('fs').readFileSync(0, 'utf8'));
if ((input.tool_input.command || '').includes('curl | sh')) {
  console.error('Nope.');
  process.exit(2);
}
```

## Debugging

- Run your hook by hand with the env vars set and a JSON payload on stdin.
- A hook that returns a non-2, non-49 error code is logged and ignored — check
  [the logs](/reference/logging) if a hook seems to do nothing.
- `option debug true` gives you the fallback and timeout log lines.

## Claude Code compatibility

Hooks are Claude Code-compatible: the same script shape, the same exit-code
semantics, and the same stdin envelope work in both.

## Full reference

The complete hook reference — including the exact config, stdin, and output
schemas — lives in
[`docs/hooks/README.md`](https://github.com/joestump-agent/crush/blob/main/docs/hooks/README.md)
in the repo, and in the built-in `crush-hooks` skill.
