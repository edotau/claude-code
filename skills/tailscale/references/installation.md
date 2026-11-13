# Installation

Standard installs: <https://tailscale.com/download>, the platform app store, or on Linux
`curl -fsSL https://tailscale.com/install.sh | sh` then `sudo tailscale up` and open the printed auth URL. The script
covers Ubuntu, Debian, RHEL, CentOS, Fedora, Raspberry Pi OS, Amazon Linux, openSUSE, Oracle Linux and VMware Photon OS.

## Linux outside the script

- **Arch Linux / NixOS**: install `tailscale` via `pacman` or the NixOS configuration.
- **Static binaries** (unsupported distributions; systemd unit in the archive's `systemd/`):

```bash
# Download from https://pkgs.tailscale.com/stable/
tar xvf tailscale_<version>_<architecture>.tgz
sudo tailscaled --state=tailscaled.state   # Start the daemon
sudo tailscale up
```

## macOS variants

| Variant | Notes |
|---|---|
| Mac App Store | GUI, sandboxed |
| Standalone (GUI) | <https://tailscale.com/download/mac>; same GUI, not sandboxed |
| Open-source CLI (`tailscaled`) | CLI only; **required for the Tailscale SSH server**; Serve/Funnel limits: [sharing-and-publishing.md](sharing-and-publishing.md) |

GUI variants reach the CLI via CLI integration or `/Applications/Tailscale.app/Contents/MacOS/Tailscale <command>`
([cli.md](cli.md)).

## Windows

<https://tailscale.com/download/windows>, or MSI for enterprise. WSL 2 is supported; see the Tailscale docs.

## Updating and uninstalling

- Update: `tailscale update` (CLI) or the app/app store; auto-update via the admin console or MDM.
- Uninstall: platform docs; on Linux `apt remove tailscale` / `yum remove tailscale`.
