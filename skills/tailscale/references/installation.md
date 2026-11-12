# Installation

Standard installs: <https://tailscale.com/download>, the platform app store, or on Linux the install script
(`curl -fsSL https://tailscale.com/install.sh | sh`, then `sudo tailscale up` and open the printed auth URL). The
script covers Ubuntu, Debian, RHEL, CentOS, Fedora, Raspberry Pi OS, Amazon Linux, openSUSE, Oracle Linux and VMware
Photon OS.

## Linux outside the script

- **Arch Linux / NixOS**: install `tailscale` through `pacman` or the NixOS configuration.
- **Static binaries** (unsupported distributions):

```bash
# Download for your architecture from https://pkgs.tailscale.com/stable/
tar xvf tailscale_<version>_<architecture>.tgz
sudo tailscaled --state=tailscaled.state   # Start the daemon
sudo tailscale up
```

A systemd service file ships in the archive's `systemd/` subdirectory.

## macOS variants

| Variant | Notes |
|---|---|
| Mac App Store | GUI, sandboxed, most common for personal use |
| Standalone (GUI) | From <https://tailscale.com/download/mac>; same GUI, not sandboxed |
| Open-source CLI (`tailscaled`) | Command-line only; **required for the Tailscale SSH server**; Funnel and Serve limits: [sharing-and-publishing.md](sharing-and-publishing.md) |

GUI variants reach the CLI via CLI integration or `/Applications/Tailscale.app/Contents/MacOS/Tailscale <command>`
([cli.md](cli.md)).

## Windows

<https://tailscale.com/download/windows>, or MSI for enterprise deployment. WSL 2 is supported; refer to the Tailscale
docs for WSL 2 instructions.

## Updating and uninstalling

- Update: `tailscale update` (CLI), the app or app store (GUI); auto-update is configurable in the admin console or
  via MDM.
- Uninstall: Tailscale's uninstall docs per platform; on Linux the package manager (`apt remove tailscale`,
  `yum remove tailscale`).
