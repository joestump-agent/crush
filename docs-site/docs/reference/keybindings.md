---
id: keybindings
title: Keybindings
sidebar_position: 3
description: Every keyboard shortcut in the Crush TUI.
---

# Keybindings

Press <kbd>ctrl+g</kbd> in the app for the live help, which always reflects the
running build.

## Global

| Keys | Does |
| --- | --- |
| <kbd>ctrl+c</kbd> | Quit |
| <kbd>ctrl+g</kbd> | More help |
| <kbd>ctrl+p</kbd> | Command palette |
| <kbd>ctrl+m</kbd> / <kbd>ctrl+l</kbd> | Model picker |
| <kbd>ctrl+s</kbd> | Session picker |
| <kbd>ctrl+z</kbd> | Suspend |
| <kbd>tab</kbd> | Change focus |
| <kbd>ctrl+y</kbd> | Toggle [yolo mode](/configuration/permissions#yolo-mode) |
:::info[Fork feature]
<kbd>tab</kbd> is three-state here — chat, editor, sidebar — because the fork
makes the [sidebar focusable](/fork#sidebar-and-header).
:::

## Editor

| Keys | Does |
| --- | --- |
| <kbd>enter</kbd> | Send |
| <kbd>shift+enter</kbd> / <kbd>ctrl+j</kbd> | Newline |
| <kbd>ctrl+o</kbd> | Open your `$EDITOR` for the prompt |
| <kbd>@</kbd> | Mention a file, or a live dispatched agent by `@handle` |
| <kbd>/</kbd> | Commands |
| <kbd>ctrl+f</kbd> | Add a file |
| <kbd>ctrl+v</kbd> / <kbd>super+v</kbd> | Paste an image from the clipboard |
| <kbd>ctrl+r</kbd> then a digit | Delete the attachment at that index |
| <kbd>ctrl+r</kbd> <kbd>r</kbd> | Delete all attachments |
| <kbd>esc</kbd> | Cancel delete mode |
| <kbd>↑</kbd> / <kbd>↓</kbd> | Prompt history |

## Chat

| Keys | Does |
| --- | --- |
| <kbd>ctrl+n</kbd> | New session |
| <kbd>ctrl+f</kbd> | Add an attachment |
| <kbd>esc</kbd> | Cancel the running turn |
| <kbd>tab</kbd> | Change focus |
| <kbd>ctrl+d</kbd> | Toggle details |
| <kbd>ctrl+t</kbd> | Toggle the pills panel (tasks, [scheduled tasks](/features/scheduled-tasks)) |

:::info[Fork feature]
The scheduled-tasks pill is fork-only, and <kbd>ctrl+t</kbd> expands *every*
pills section rather than only the first.
:::

### Navigation

| Keys | Does |
| --- | --- |
| <kbd>↑</kbd> / <kbd>↓</kbd> | Scroll |
| <kbd>shift+↑</kbd> / <kbd>K</kbd> | Up one item |
| <kbd>shift+↓</kbd> / <kbd>J</kbd> | Down one item |
| <kbd>u</kbd> / <kbd>d</kbd> | Half page up / down |
| <kbd>b</kbd> / <kbd>pgup</kbd> | Page up |
| <kbd>f</kbd> / <kbd>space</kbd> / <kbd>pgdn</kbd> | Page down |
| <kbd>g</kbd> / <kbd>home</kbd> | Home |
| <kbd>G</kbd> / <kbd>end</kbd> | End |
| <kbd>ctrl+end</kbd> | End and follow |
| <kbd>h</kbd> / <kbd>←</kbd> | Focus chat |
| <kbd>l</kbd> / <kbd>→</kbd> | Focus sidebar (fork: the sidebar takes focus, scrolls with the mouse, and hides irrelevant help while focused) |
| <kbd>shift+←</kbd> / <kbd>H</kbd> | Scroll left |
| <kbd>shift+→</kbd> / <kbd>L</kbd> | Scroll right |

### Selection

| Keys | Does |
| --- | --- |
| <kbd>c</kbd> / <kbd>y</kbd> | Copy |
| <kbd>space</kbd> | Expand / collapse the focused item |
| <kbd>esc</kbd> | Clear selection |

Finished assistant and user messages also carry a click-to-copy icon, and plain
clicks on hyperlinks open them in your browser.

:::info[Fork feature]
Both the click-to-copy icon and click-to-open hyperlinks are fork additions.
:::

## Inspect mode

:::info[Fork feature]
Inspect mode is part of [multi-agent dispatch](/agents/overview). See
[Handles and inspect](/agents/handles-and-inspect).
:::

Read a sub-agent's full transcript — reasoning, tool calls, results — in the
chat window while the parent stays the active session. These keys work from
both the editor and the chat; an open dialog keeps its own keys.

| Keys | Does |
| --- | --- |
| <kbd>ctrl+]</kbd> | On a focused agent block, open that agent's transcript. With no block focused, open the first live agent. While inspecting, cycle to the next live agent |
| <kbd>ctrl+[</kbd> | Return to the chat, with the scroll position restored |
| <kbd>ctrl+x</kbd> | Cancel the selected live dispatch card, or the dispatch you are inspecting. Runs end killed with reason "canceled by user"; the workspace is kept. Shown in the <kbd>ctrl+g</kbd> help only while a live dispatch is targeted |
| <kbd>esc</kbd> | Return to the chat (same as <kbd>ctrl+[</kbd>) |
| <kbd>ctrl+]</kbd> in the session picker | Open the selected session's sub-agent list. See [Sessions](/features/sessions#sub-agent-sessions) |

A live agent's transcript follows its stream. Prompts you type while
inspecting go to the parent session, never to the agent — to steer a running
agent, start the prompt with its `@handle`. The cycle covers the agents that
were live when you entered; agents dispatched later join on your next entry.
The drill and back keys appear in the status-bar help and the <kbd>ctrl+g</kbd>
help whenever they apply: <kbd>ctrl+]</kbd> while an agent block is selected,
and <kbd>esc</kbd> (plus <kbd>ctrl+]</kbd> "next agent" when several agents are
live) while inspecting.

## Initialization prompt

| Keys | Does |
| --- | --- |
| <kbd>y</kbd> | Yes |
| <kbd>n</kbd> / <kbd>esc</kbd> | No |
| <kbd>tab</kbd> / <kbd>←</kbd> / <kbd>→</kbd> | Switch |
| <kbd>enter</kbd> | Select |

## Composer completions

Typing certain characters opens an inline completion list:

| Trigger | Completes |
| --- | --- |
| <kbd>@</kbd> | Files in the project, and live [dispatched agents](/agents/handles-and-inspect) by handle, each shown with its role, status, and current todo |
| <kbd>/</kbd> | Commands, [user-invocable skills](/features/skills#user-invocable-skills), and MCP prompts |

A prompt that *starts* with a live agent's `@handle` is delivered to that agent
instead of the main agent; a `@handle` later in the prompt attaches the agent's
status card. A handle-shaped token (no `.` or `/`) that matches a dispatched
agent wins over a file of the same name.

:::info[Fork feature]
Skill, MCP-prompt, and agent completions, `@handle` routing, the token
highlighting that makes them visually distinct, and the atomic-backspace
behaviour below are all fork additions. Upstream completes files and commands
only.
:::

Completion list size is tunable:

```bash
option ui completions-max-depth 4
option ui completions-max-items 200
```

Backspace over a completed token deletes the whole token — and drops its
attachment, if it had one — rather than eating one character at a time.
