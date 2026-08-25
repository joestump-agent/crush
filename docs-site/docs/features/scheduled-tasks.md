---
id: scheduled-tasks
title: Scheduled tasks
sidebar_position: 8
description: Cron-driven prompts — one-shot reminders and recurring jobs the agent schedules for itself.
---

# Scheduled tasks

:::info[Fork feature]
Scheduled tasks are an addition in the `joestump-agent/crush` fork.
:::

Crush can re-run a prompt on a cron schedule. You ask for it in plain English —
*"remind me in five minutes to check CI"*, *"every morning at 9, summarise
what's on main"* — and the agent creates the task itself with the `CronCreate`
tool.

Scheduled tasks show up in the pills panel (<kbd>ctrl+t</kbd>) with their next
fire time.

## The tools

| Tool | Does |
| --- | --- |
| `CronCreate` | Schedule a prompt on a cron expression |
| `CronList` | List every task in the session, with schedule, prompt, recurrence, durability, run count, and next fire time |
| `CronDelete` | Cancel a task by ID |

They mirror the `CronCreate` / `CronList` / `CronDelete` tools in Claude Code
and Codex, so a model that knows those knows these.

## Cron expressions

A standard 5-field expression — minute, hour, day-of-month, month, day-of-week —
evaluated in **your local timezone**.

Supported:

| Syntax | Example | Means |
| --- | --- | --- |
| Wildcard | `*`, `?` | Every value |
| Single value | `5` | Just that one |
| Step | `*/15` | Every 15 |
| Range | `1-5` | Inclusive range |
| List | `1,15,30` | Any of these |
| Names | `JAN`, `MON` | Three-letter month and day names |

Day-of-week runs `0` or `7` = Sunday through `6` = Saturday. When **both**
day-of-month and day-of-week are restricted, the task fires when **either**
matches.

Not supported: a seconds field, descriptors like `@daily`, and extended syntax
(`L`, `W`).

## Timing

Tasks fire at the **top** of the specified minute. Pinning the current minute at
06:22:50 means the task fires at 06:23:00, not ten seconds later. There is no
sub-minute precision.

## Recurring vs one-shot

| `recurring` | Behaviour |
| --- | --- |
| `true` | Fires on every match, forever, until deleted |
| `false` | Fires once and deletes itself. Pin minute, hour, day-of-month, and month to specific values. |

## Durability

`durable: true` persists the task to disk so it survives a restart. Otherwise it
lives only in the current session.

A session can hold up to **50** scheduled tasks.

## What gets rejected

The scheduler refuses schedules that would silently misbehave rather than
accepting them:

- **Expressions that can never match.** `0 0 30 2 *` — February 30th — is
  rejected, not accepted as a task that never runs.
- **One-shots in the past.** A `recurring: false` task whose pinned time has
  already passed today is rejected, because its next match would jump to
  tomorrow or next year, which is never what a one-shot means. Recurring
  schedules are unaffected: a daily 9am task created at 10:30 legitimately fires
  tomorrow.
- **Stale-clock schedules.** The `<env>` block in the system prompt carries the
  session's *start* time, which goes stale as a session runs. The tool tells the
  model to call `date` before computing cron fields, so that a schedule built
  from the stale time doesn't land in the past.

## Common patterns

| Ask | Expression | Recurring |
| --- | --- | --- |
| "Remind me in 5 minutes" | current minute + 5, with rollover | `false` |
| "Check every hour" | `0 * * * *` | `true` |
| "Every day at 9am" | `0 9 * * *` | `true` |
| "At 2:30pm today" | `30 14 <today_dom> <today_month> *` | `false` |
| "Every 15 minutes" | `*/15 * * * *` | `true` |
| "Weekday mornings" | `0 8 * * MON-FRI` | `true` |

## Managing them

```text
> what's scheduled?
> cancel the CI check
```

`CronList` returns each task's ID; `CronDelete` takes one. Fired one-shots are
cleared from the pill automatically.
