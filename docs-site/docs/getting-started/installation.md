---
id: installation
title: Installation
sidebar_position: 1
description: Build the joestump-agent/crush fork from source — the only way to get it today.
---

# Installation

:::warning[Read this before you install anything]
These docs cover the
**[`joestump-agent/crush`](https://github.com/joestump-agent/crush) fork**, and
the fork is **not published to any package manager**. Every published Crush
package — Homebrew, npm, winget, scoop, apt, the NUR, the GitHub releases —
installs **upstream Charm Crush**, which does not have
[Sidekick](/features/sidekick), [A2UI](/features/a2ui),
[scheduled tasks](/features/scheduled-tasks),
[semantic search](/features/semantic-search), or the fork's
[channel additions](/features/channels).

`go install github.com/charmbracelet/crush@latest` is the same trap: the fork
keeps upstream's module path, so that command resolves to Charm's repository
regardless of which fork you meant. It will succeed, and you will silently have
the wrong binary.

**Build from source. It is the only way to get this Crush.**
:::

## Prerequisites

- **Go 1.26.5 or newer** — the toolchain is the only build dependency.
- **git**.

```bash
go version   # must be >= go1.26.5
```

## Install

```bash
git clone https://github.com/joestump-agent/crush.git
cd crush
go install .
```

`go install .` builds the module in the current directory and drops a `crush`
binary in `$GOBIN` (or `$GOPATH/bin`, `~/go/bin` by default). Make sure that
directory is on your `PATH`:

```bash
export PATH="$(go env GOPATH)/bin:$PATH"
```

Prefer to control where the binary lands? Build it in place instead:

```bash
go build -o ~/.local/bin/crush .
```

## Verify you got the fork

```bash
crush --version
```

The surest check is a fork-only feature — press <kbd>ctrl+a</kbd> in a session.
If the [Sidekick](/features/sidekick) panel opens, you are on the fork. If
nothing happens, you are running upstream Crush.

## Updating

```bash
cd crush
git pull
go install .
```

There is no release channel and no auto-update. Pull when you want the latest.

## Platform support

Crush stores its local database with
[modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite), so it builds on the
platforms that driver supports — macOS, Linux, Windows, FreeBSD, OpenBSD,
NetBSD, and Android. The illumos and Solaris family is not among them and no
longer builds.

## Clipboard support

Copy and paste needs an external helper on some Unix-like systems:

| Environment | Tool |
| --- | --- |
| macOS | Native support |
| Windows | Native support |
| Linux/BSD + Wayland | `wl-copy` and `wl-paste` |
| Linux/BSD + X11 | `xclip` or `xsel` |

## Working on Crush itself

The repo uses [Task](https://taskfile.dev) for its development targets:

```bash
task build     # build the binary
task test      # go test -race ./...
task lint      # golangci-lint
task docs:dev  # this documentation site
```

:::warning
`lint` is not enforced by the fork's CI — `lint.yml` delegates to
`charmbracelet/meta`'s reusable workflow, which does not run here. Run
`task lint` locally before you push; the `build` matrix is the only gating
check.
:::

## Looking for upstream Crush?

If you want Charm's Crush rather than this fork — the packaged, released,
supported one — its installation instructions are in the
[upstream README](https://github.com/charmbracelet/crush#installation). Nothing
on this site applies to it except by coincidence.

## Next

Head to the [Quickstart](/getting-started/quickstart).
