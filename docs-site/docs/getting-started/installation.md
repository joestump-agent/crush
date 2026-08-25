---
id: installation
title: Installation
sidebar_position: 1
description: Install Crush with a package manager, from a release binary, or from source.
---

# Installation

## Package managers

```bash
# Homebrew
brew install charmbracelet/tap/crush

# NPM
npm install -g @charmland/crush

# Arch Linux (btw)
yay -S crush-bin

# Nix
nix run github:numtide/nix-ai-tools#crush

# FreeBSD
pkg install crush
```

Windows:

```powershell
# Winget
winget install charmbracelet.crush

# Scoop
scoop bucket add charm https://github.com/charmbracelet/scoop-bucket.git
scoop install crush
```

## Debian / Ubuntu

```bash
sudo mkdir -p /etc/apt/keyrings
curl -fsSL https://repo.charm.sh/apt/gpg.key \
  | sudo gpg --dearmor -o /etc/apt/keyrings/charm.gpg
echo "deb [signed-by=/etc/apt/keyrings/charm.gpg] https://repo.charm.sh/apt/ * *" \
  | sudo tee /etc/apt/sources.list.d/charm.list
sudo apt update && sudo apt install crush
```

## Fedora / RHEL

```bash
echo '[charm]
name=Charm
baseurl=https://repo.charm.sh/yum/
enabled=1
gpgcheck=1
gpgkey=https://repo.charm.sh/yum/gpg.key' | sudo tee /etc/yum.repos.d/charm.repo
sudo yum install crush
```

## Nix (NUR)

Crush is available via the official Charm
[NUR](https://github.com/nix-community/NUR) in
`nur.repos.charmbracelet.crush`, which is the most up-to-date way to get Crush
in Nix.

```bash
# Add the NUR channel.
nix-channel --add https://github.com/nix-community/NUR/archive/main.tar.gz nur
nix-channel --update

# Get Crush in a Nix shell.
nix-shell -p '(import <nur> { pkgs = import <nixpkgs> {}; }).repos.charmbracelet.crush'
```

NixOS and Home Manager modules ship via NUR as well. The module auto-detects
which context it is in, so the import is identical either way:

```nix
{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    nur.url = "github:nix-community/NUR";
  };

  outputs = { self, nixpkgs, nur, ... }: {
    nixosConfigurations.your-hostname = nixpkgs.lib.nixosSystem {
      modules = [
        nur.modules.nixos.default
        nur.repos.charmbracelet.modules.crush
        {
          programs.crush = {
            enable = true;
            settings = {
              # …see the Configuration section.
            };
          };
        }
      ];
    };
  };
}
```

## Release binaries

Prebuilt binaries for every supported platform are attached to each
[upstream release](https://github.com/charmbracelet/crush/releases). Download,
unpack, and put `crush` on your `PATH`.

## From source

Crush is a Go program with no build-time dependencies beyond the Go toolchain.

```bash
go install github.com/charmbracelet/crush@latest
```

Crush stores its local database with
[modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite), so it builds on the
platforms that driver supports — every platform binaries are published for, plus
Android. The illumos and Solaris family is not among them and no longer builds.

## Building this fork

The [fork](https://github.com/joestump-agent/crush) is not published to any
package manager — build it from source:

```bash
git clone https://github.com/joestump-agent/crush.git
cd crush
go build -o crush .
```

The repo uses [Task](https://taskfile.dev) for its development targets:

```bash
task build     # build the binary
task test      # go test -race ./...
task lint      # golangci-lint
```

:::warning
`lint` is not enforced by the fork's CI, so run `task lint` locally before you
push. The `build` matrix is the only gating check.
:::

## Clipboard support

Copy and paste needs an external helper on some Unix-like systems:

| Environment | Tool |
| --- | --- |
| macOS | Native support |
| Windows | Native support |
| Linux/BSD + Wayland | `wl-copy` and `wl-paste` |
| Linux/BSD + X11 | `xclip` or `xsel` |

## Next

Head to the [Quickstart](/getting-started/quickstart).
