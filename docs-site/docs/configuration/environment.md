---
id: environment
title: Environment and startup
sidebar_position: 7
description: Setting environment variables from config, and the startup options that change how Crush behaves.
---

# Environment and startup

## Setting variables from config

The top-level `env` field sets environment variables at startup, **before**
providers are configured. This is the way to set variables that affect provider
authentication — the AWS SDK credential chain, for example — without wrapping
`crush` in a shell script or exporting them in your shell profile:

```json
{
  "$schema": "https://charm.land/crush.json",
  "env": {
    "AWS_PROFILE": "my-sso-profile"
  }
}
```

Values support the same `$VAR` and `$(command)` expansion as other config
fields.

In a [`crushrc`](/configuration/crushrc), of course, you just export them:

```bash
export AWS_PROFILE=my-sso-profile
```

## Notifications

Crush sends desktop notifications when a tool call needs permission and when the
agent finishes its turn. They only fire when the terminal window is **not**
focused *and* your terminal supports reporting focus state.

```bash
# Choose auto, native, osc, bell, or disabled.
option notifications auto
```

`auto` uses native notifications locally and OSC notifications over SSH when
supported. See [Notifications](/features/notifications).

## Attribution

By default Crush adds attribution to Git commits and pull requests it creates.

```bash
option attribution-trailer-style co-authored-by
option attribution-generated-with true
```

| `attribution-trailer-style` | Adds |
| --- | --- |
| `assisted-by` (default) | `Assisted-by: Crush:[ModelID]`, per [the kernel convention](https://docs.kernel.org/process/coding-assistants.html#attribution) |
| `co-authored-by` | `Co-Authored-By: Crush <crush@charm.land>` |
| `none` | No trailer |

`attribution-generated-with` (default `true`) adds a
`💘 Generated with Crush` line to commit messages and PR descriptions. Set it to
`false` to suppress that.

## The terminal UI

```bash
option ui compact true                 # compact chat layout
option ui diff unified                 # or: split
option ui transparent true             # use the terminal background
option ui scrollbar always             # default | always | never
option ui completions-max-depth 4
option ui completions-max-items 200
```

## Metrics

Crush records pseudonymous usage metrics tied to a device-specific hash. The
metrics are usage metadata only — **prompts and responses are never collected**.
The exact fields are visible in the source, under
[`internal/event`](https://github.com/charmbracelet/crush/tree/main/internal/event).

Opt out either way:

```bash
export CRUSH_DISABLE_METRICS=1
export DO_NOT_TRACK=1          # the donottrack.sh convention
```

or in config:

```bash
option metrics false
```

## Debugging

```bash
crush --debug
```

or persistently:

```bash
# crushrc
option debug true
option debug-lsp true
```

See [Logging](/reference/logging).

## Full variable list

Every environment variable Crush reads is in the
[environment variable reference](/reference/environment-variables).
