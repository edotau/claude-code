# Tailscale CLI

For agents operating `tailscale` on the user's machine (Linux, macOS, Windows; no CLI on iOS/Android). Fallbacks:

1. **`tailscale help <subcommand>`** — always current; use it for any unlisted flag or an unknown-flag failure, and
   verify unfamiliar flags before suggesting them.
2. **<https://tailscale.com/docs/reference/tailscale-cli>** — when `tailscale help` isn't available.

## Operating rules

- **Decision logic**: `tailscale status --json | jq ...` (stable shape; the table format isn't).
- **Resolve, don't hard-code**: `tailscale ip <hostname>`; IPs can change on re-auth.
- **Confirm before destructive actions**: `logout` unregisters the device (`down` only disconnects); `lock disable`
  needs the recovery secret and weakens security; `set --reset`/`up --reset` revert unspecified flags to defaults.
- **Privilege**: most `set`/`up` need root on Linux, or `tailscale set --operator=$USER`. On macOS/Windows the GUI
  client typically owns the daemon; CLI changes may need elevation.
- **Not installed**: don't fabricate output; say so and see [installation.md](installation.md).

## CLI location by platform

- **macOS (standalone)**: **Settings > CLI integration > Install Now** (macOS 13+) installs `/usr/local/bin/tailscale`.
- **macOS (App Store)**: `/Applications/Tailscale.app/Contents/MacOS/Tailscale <command>`; set `TAILSCALE_BE_CLI=1` in
  scripts to force CLI mode.
- **Linux / Windows**: on `$PATH`.

## Connection & authentication

```bash
tailscale up --auth-key=tskey-auth-xxxxx  # Headless (servers, CI)
tailscale up --login-server=https://...   # Custom control server
tailscale switch --list                   # List profiles (Fast User Switching)
tailscale switch <tailnet>                # Switch profile
```

`tailscale up` flags: `--accept-routes` (accept subnet routes; Linux), `--accept-dns` (default true), `--hostname`,
`--shields-up` (block incoming), `--force-reauth`, `--reset` (reset unspecified settings), `--advertise-tags`
(must be pre-authorized), `--timeout` (max login wait; default 0 = forever).

## Status & diagnostics

```bash
tailscale status --peers=false  # Only this device
tailscale ip -4                 # This device's IPv4 (-6 for IPv6; `ip <host>` for a peer)
tailscale whois 100.64.1.2      # Owner, hostname, tags of a Tailscale IP
tailscale version --daemon      # Daemon version (may differ from client)
tailscale netcheck              # NAT mapping type, UPnP/NAT-PMP/PCP, preferred DERP, DERP latencies
tailscale ping <host>           # TSMP ping; direct vs DERP-relayed
tailscale ping --icmp <host>    # ICMP through the tunnel (also --tsmp, --peerapi)
tailscale nc <host> <port>      # Netcat-like TCP test
tailscale dns status            # DNS configuration
tailscale dns query <name>      # Look up a name via Tailscale DNS
```

Flow: `status` (online? peers visible?) → `ping` (direct or DERP?) → `netcheck` (NAT type, UDP) → `status --json`. A
DERP-relayed ping usually means UDP is blocked or a side is behind a strict NAT; `netcheck` confirms. Background:
[connectivity.md](connectivity.md).

## Configuration (`tailscale set`)

Changes config without reconnecting; accepts most `up` flags (`tailscale help set`).

```bash
tailscale set --ssh                          # Tailscale SSH server
tailscale set --advertise-exit-node          # Advertise as exit node
tailscale set --exit-node=<ip-or-hostname>   # Use an exit node
tailscale set --exit-node=                   # Stop using it
tailscale set --exit-node-allow-lan-access   # LAN access while using exit node
tailscale set --advertise-routes=10.0.0.0/24 # Advertise subnet routes
tailscale set --accept-routes                # Accept routes (Linux)
tailscale set --operator=$USER               # Non-root management
tailscale set --auto-update
tailscale set --webclient                    # Web client interface
tailscale set --advertise-connector          # App connector
```

## Serve & Funnel

```bash
tailscale serve --bg 3000                                 # Proxy local :3000 over HTTPS :443 (--bg persists)
tailscale serve --bg --set-path=/docs /path/to/files      # Serve a directory under /docs
tailscale serve --bg --set-path=/health text:OK           # Static text
tailscale serve --bg --tcp=5432 tcp://localhost:5432      # Raw TCP
tailscale serve --bg --tls-terminated-tcp=5432 tcp://localhost:5432
tailscale serve https+insecure://localhost:8443           # Upstream with a self-signed cert
tailscale serve status
tailscale serve --https=443 --set-path=/docs off          # Remove a handler
tailscale serve reset                                     # Reset all

tailscale funnel --bg 3000                                # Same, but public
tailscale funnel status
tailscale funnel reset
```

Serve provisions a TLS cert for the device FQDN. Funnel limits: [sharing-and-publishing.md](sharing-and-publishing.md).

## Files

```bash
tailscale file cp photo.jpg my-laptop:   # Taildrop send — trailing colon
tailscale file get /path/to/download/dir # Receive (waits for transfers)

tailscale drive share docs /home/user/Documents  # Taildrive share
tailscale drive list
tailscale drive rename docs documents
tailscale drive unshare docs
```

Taildrive shares appear at `\\machine\tailscale\share-name` (Windows) or via WebDAV.

## Security & administration

- `tailscale lock ...` — Tailnet Lock; see [connectivity.md](connectivity.md).
- `tailscale cert machine.tailnet-name.ts.net` — writes `.crt`/`.key`; auto-renewed.
- `tailscale update [--check | --yes | --track=stable|unstable]`.
- `tailscale bugreport` — prints a bug report ID.
- `tailscale configure kubeconfig <hostname>` / `tailscale configure synology`.
- `tailscale metrics print` (Prometheus text) / `tailscale metrics write <path>`.
- `tailscale syspolicy list` / `reload` — MDM-managed values.
- `tailscale completion bash|zsh|fish|powershell`, e.g. `tailscale completion zsh > "${fpath[1]}/_tailscale"`.
