---
id: skills
title: Skills
sidebar_position: 6
description: The Agent Skills open standard in Crush — the four built-in skills, every discovery path, and how to write your own.
---

# Skills

Crush supports the [Agent Skills](https://agentskills.io) open standard for
extending agent capabilities with reusable instruction packages. A skill is a
folder containing a `SKILL.md` file that Crush discovers and activates on
demand.

Skills are cheap: the model sees only each skill's `name` and `description`
until it decides one is relevant, then loads the full instructions.

## Built-in skills

Four skills are embedded in the Crush binary and are always available with no
configuration. They live at `crush://skills/<name>/SKILL.md`.

### `crush-config`

> Use when the user needs help configuring Crush — writing `crushrc` (the Bash
> config format) or `crush.json`, setting up providers, models, LSPs, MCP
> servers, hooks, skills, permissions, or changing Crush behavior.

This is the one that makes "just tell Crush what you want configured" work. It
carries the whole [builtin command reference](/configuration/command-reference),
the discovery-and-merge rules, the hooks runtime, user-invocable skill
frontmatter, and the legacy JSON mapping.

### `crush-hooks`

> Use when the user wants to add, write, debug, or configure a Crush hook —
> gating or blocking tool calls, approving or rewriting tool input before
> execution, injecting context into tool results, or troubleshooting hook
> behavior.

Covers the supported events, the input envelope, exit-code semantics, the
JSON output envelope, aggregation across multiple hooks, canonical examples, an
authoring checklist, and Claude Code compatibility. See
[Hooks](/features/hooks).

### `a2ui`

> Use when the user asks you to output, speak in, or communicate using the A2UI
> (a2tea) format, or when you need to understand how to construct A2UI JSON
> components to render interactive terminal UIs.

Teaches both delivery paths (MCP `/a2ui` resource vs. inline `<a2ui-json>`), the
component catalog, the form submission loop, the A2UI-over-MCP contract, and the
anti-patterns. See [A2UI](/features/a2ui).

### `jq`

> Use when the user needs to query, filter, reshape, extract, create, or
> construct JSON data — including API responses, config files, log output — or
> when helping the user write or debug JSON transformations.

Crush ships a **built-in `jq`** command (via
[gojq](https://github.com/itchyny/gojq)) available inside the `bash` tool. No
external binary is required, on any platform.

Supported flags include `-r`/`--raw-output`, `-j`/`--join-output`,
`-c`/`--compact-output`, `-s`/`--slurp`, and `-n`/`--null-input`.

Differences from C jq, because it is gojq:

- Object keys are **sorted** by default; `keys_unsorted` and `-S` are
  unavailable.
- Integers are arbitrary precision.
- String indexing works: `"abcde"[2]` is `"c"`.
- Not supported: `--ascii-output`, `--seq`, `--stream`, `--stream-errors`,
  `-f`/`--from-file`, `--slurpfile`, `--rawfile`, `--args`, `--jsonargs`,
  `input_line_number`, `$__loc__`, and some regex features (backreferences,
  look-around).
- gojq supports `--yaml-input`/`--yaml-output`, but the built-in does not expose
  those flags.

:::info[Fork feature]
In this fork all four built-in skills are marked `user-invocable`, so they also
appear in the command palette (<kbd>ctrl+p</kbd>) as `user:` entries.
:::

## Where Crush looks for skills

**Global paths:**

- `$CRUSH_SKILLS_DIR`
- `$XDG_CONFIG_HOME/agents/skills` or `~/.config/agents/skills/`
- `$XDG_CONFIG_HOME/crush/skills` or `~/.config/crush/skills/`
- `~/.agents/skills/`
- `~/.claude/skills/`

On Windows, additionally:

- `%LOCALAPPDATA%\agents\skills\` or `%USERPROFILE%\AppData\Local\agents\skills\`
- `%LOCALAPPDATA%\crush\skills\` or `%USERPROFILE%\AppData\Local\crush\skills\`

**Project-relative paths**, loaded automatically — you do **not** need to
configure these:

- `.agents/skills`
- `.crush/skills`
- `.claude/skills`
- `.cursor/skills`

**Additional paths** you declare:

```bash
option skill-path "$HOME/squid-skills" "./other-skills"
```

## Getting some skills

The [anthropics/skills](https://github.com/anthropics/skills) repo is a good
starting set:

```bash
# Unix
mkdir -p ~/.config/crush/skills
cd ~/.config/crush/skills
git clone https://github.com/anthropics/skills.git _temp
mv _temp/skills/* . && rm -rf _temp
```

```powershell
# Windows (PowerShell)
mkdir -Force "$env:LOCALAPPDATA\crush\skills"
cd "$env:LOCALAPPDATA\crush\skills"
git clone https://github.com/anthropics/skills.git _temp
mv _temp/skills/* . ; rm -r -force _temp
```

## Writing a skill

A skill is a directory with a `SKILL.md`:

```text
my-hot-skill/
└── SKILL.md
```

```markdown
---
name: my-hot-skill
description: Use when the user needs to do the specific thing this skill knows how to do.
---

# My Hot Skill

Instructions the model should follow when this skill activates.
```

### Frontmatter

| Field | Type | Meaning |
| --- | --- | --- |
| `name` | string | Required. Lowercase alphanumeric with single hyphens (`my-hot-skill`). |
| `description` | string | Required. This is the *only* thing the model sees before activating, so write it as a trigger: "Use when…". |
| `user-invocable` | bool | Show the skill in the command palette. |
| `disable-model-invocation` | bool | Hide it from the model's skill list; user invocation still works. |
| `license` | string | Optional. |
| `compatibility` | string | Optional. |
| `metadata` | map | Optional string map. |

### User-invocable skills

```yaml
---
name: my-hot-skill
description: A skill that can be invoked as a command.
user-invocable: true
---
```

These appear in the command palette (<kbd>ctrl+p</kbd>) with a prefix showing
where they came from:

- `user:skill-name` — from a global directory
- `project:skill-name` — from a project directory

Invoking one loads the skill's instructions into the conversation.

To keep the model from auto-triggering a skill while leaving it available to
you:

```yaml
---
name: my-skill
description: Only invocable by users, not the model.
user-invocable: true
disable-model-invocation: true
---
```

Skills with `disable-model-invocation` don't appear in the model's available
skills list at all.

## Disabling skills

Hide a skill from the agent entirely — built-in skills included:

```bash
option disable-skill crush-config
option disable-skill jq
```

## Seeing what loaded

The `/skills` dialog lists every discovered skill, where it came from, and any
that failed to parse — with the error and its source labelled. Skills that fail
validation are reported rather than silently dropped.

Ask the agent for the same thing at any time; the `crush_info` tool reports
active and available skills alongside model, LSP, MCP, hook, and permission
state.
