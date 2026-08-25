---
id: notifications
title: Desktop notifications
sidebar_position: 12
description: When Crush pings you, which transport it uses, and how to turn it off.
---

# Desktop notifications

Crush sends desktop notifications when:

- a tool call requires permission, and
- the agent finishes its turn.

They are only sent when the terminal window is **not focused** *and* your
terminal supports reporting focus state. If your terminal doesn't report focus,
you get nothing — that's the terminal, not Crush.

## Choosing a transport

```bash
# crushrc
option notifications auto
```

| Value | Behaviour |
| --- | --- |
| `auto` | Native notifications locally; OSC notifications over SSH where supported |
| `native` | Always use the OS notification system |
| `osc` | Always use terminal OSC escape sequences (OSC 99/777, auto-detected) |
| `bell` | Terminal bell |
| `disabled` | Nothing |

`auto` is the one that does the right thing over SSH: a native notification
would fire on the *remote* machine, which is not where you are, so it falls back
to OSC and lets your local terminal emulator raise it.

## Turning them off

```bash
option notifications disabled
```
