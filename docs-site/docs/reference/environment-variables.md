---
id: environment-variables
title: Environment variables
sidebar_position: 4
description: Every environment variable Crush reads, grouped by what it affects.
---

# Environment variables

## Paths and directories

| Variable | Effect |
| --- | --- |
| `CRUSH_GLOBAL_CONFIG` | Override the global config directory |
| `CRUSH_GLOBAL_DATA` | Override the global data directory |
| `CRUSH_CACHE_DIR` | Override the cache directory |
| `CRUSH_SKILLS_DIR` | An additional [skills](/features/skills) directory |
| `XDG_CONFIG_HOME`, `XDG_DATA_HOME` | Standard XDG bases Crush resolves against |

Run `crush dirs` to see what these actually resolved to.

## Behaviour

| Variable | Effect |
| --- | --- |
| `CRUSH_ALLOW_COMMANDS` | Comma-separated commands to remove from the [bash blocklist](/configuration/permissions#blocked-commands). `--allow-commands` wins. |
| `CRUSH_ALLOW_ALL_COMMANDS` | Remove **all** blocklist restrictions (dangerous). `--allow-all-commands` wins. |
| `CRUSH_DISABLE_METRICS` | Opt out of [usage metrics](/configuration/environment#metrics) |
| `DO_NOT_TRACK` | The [donottrack.sh](https://donottrack.sh) convention; same effect |
| `CRUSH_DISABLE_PROVIDER_AUTO_UPDATE` | Stop checking Catwalk for provider updates |
| `CRUSH_DISABLE_DEFAULT_PROVIDERS` | Don't include the built-in provider catalog |
| `CRUSH_DISABLE_ANTHROPIC_CACHE` | Disable Anthropic prompt caching |
| `CRUSH_CORE_UTILS` | Force the Go coreutils implementations on (`true`) or off (`false`) in the embedded shell. Defaults to on for Windows, off elsewhere. |
| `CRUSH_VERSION` | Set by Crush inside a `crushrc`, for [version-conditional config](/configuration/crushrc#config-versioning) |

:::info[Fork feature]
`CRUSH_ALLOW_COMMANDS` and `CRUSH_ALLOW_ALL_COMMANDS` are fork additions — see
[Permissions](/configuration/permissions#blocked-commands). Everything else in
this table is upstream.
:::

## Server

| Variable | Effect |
| --- | --- |
| `CRUSH_SERVER_IDLE_TIMEOUT` | Seconds the server lingers after its last workspace is released (default 60; `0` shuts down immediately) |
| `CRUSH_SERVER_DETACH_GRACE` | Seconds a client's workspace claim survives a dropped stream (default 10; `0` tears down immediately) |
| `CRUSH_SERVER_READY_TIMEOUT` | Go duration bounding how long a client waits for a server to become ready |
| `CRUSH_CLIENT_SERVER` | Truthy to make the default `crush` invocation connect to a server process |

See [Server and workspaces](/features/server-and-workspaces).

## Hooks and the bash tool

Set **by** Crush, read by your scripts. See [Hooks](/features/hooks#input).

| Variable | Value |
| --- | --- |
| `CRUSH` | Always `1` |
| `AGENT` | Always `crush` |
| `AI_AGENT` | Always `crush` |
| `CRUSH_EVENT` | The hook event name |
| `CRUSH_TOOL_NAME` | The tool being called |
| `CRUSH_SESSION_ID` | Current session ID |
| `CRUSH_CWD` | Working directory |
| `CRUSH_PROJECT_DIR` | Project root |
| `CRUSH_TOOL_INPUT_COMMAND` | For `bash`: the shell command |
| `CRUSH_TOOL_INPUT_FILE_PATH` | For file tools: the target path |

`CRUSH`, `AGENT`, and `AI_AGENT` are set in both the `bash` tool and hooks, so
"am I running under an AI agent?" detection behaves the same in either context.

## Provider authentication

Every provider key is in
[Providers and API keys](/getting-started/providers-and-api-keys).

`CRUSH_HYPER_API_KEY` and `HYPER_API_KEY` both authenticate
[Charm Hyper](https://hyper.charm.land).

## Development and debugging

These exist for working on Crush itself. They are not part of the supported
surface and may change without notice.

| Variable | Effect |
| --- | --- |
| `CRUSH_UI_DEBUG` | Set to `true` for extra TUI debug output |
| `CRUSH_SKIP_DATADIR_LOCK` | Skip the data-directory lock |
| `CRUSH_CALLBACK_PREVIEW` | OAuth callback preview |
| `CRUSH_BENCH_DATADIR`, `CRUSH_BENCH_SESSION` | Benchmark harness |
