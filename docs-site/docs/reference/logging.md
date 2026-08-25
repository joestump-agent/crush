---
id: logging
title: Logging
sidebar_position: 5
description: Where Crush logs, how to read them, and how to turn up the volume.
---

# Logging

Crush logs to `./.crush/logs/crush.log`, relative to the project.

## Reading logs

```bash
# Print the last 1000 lines (the default).
crush logs

# Print the last 500 lines.
crush logs --tail 500

# Follow in real time.
crush logs --follow
```

The tail default is 1000 for performance — a long-running project accumulates a
lot.

## More volume

```bash
crush --debug
```

or persistently:

```bash
# crushrc
option debug true
option debug-lsp true
```

`debug-lsp` is separate because LSP traffic is verbose enough to bury everything
else. Turn it on when diagnostics look wrong, off otherwise.

## Server logs

A `crush server` writes to a **different** file: a per-host log under the cache
directory, not the project's `.crush/logs/`. Find it with:

```bash
crush dirs
```

## Asking the agent

The `crush_logs` tool lets the agent read Crush's own logs, which is the fastest
way to debug Crush behaviour from inside a session:

```text
> check the logs — why did the gopls LSP fail to start?
```

## What is in there

Hook fallbacks and timeouts, MCP connection failures and init timeouts, LSP
start-up and restarts, config discovery and merge warnings, provider catalog
updates, and workspace flag mismatches in a
[shared workspace](/features/server-and-workspaces#first-wins-flags).
