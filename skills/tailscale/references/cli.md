# Tailscale CLI

For agents operating `tailscale` on the user's machine (Linux, macOS, Windows; no CLI on iOS/Android). Fallbacks, in
order:

1. **`tailscale help <subcommand>`** — always current, no network round-trip. Use it for any flag not listed here,
   or when an example fails with an unknown flag (the CLI evolves).
2. **<https://tailscale.com/docs/reference/tailscale-cli>** — when `tailscale help` isn't available (before install,
   or documenting another platform).

Before suggesting an unfamiliar flag or subcommand from memory, verify it with `tailscale help <command>`.

## Operating rules

- **Prefer `tailscale status --json | jq ...`** for any decision logic (select a device, check online status, find a
  peer's IP); the JSON shape is stable, the table format isn't.
- **Resolve, don't hard-code**: `tailscale ip <hostname>` returns a peer's current IP; hostnames are stable, IPs can
  change on re-auth.
- **Confirm before destructive actions**: `logout` unregisters the device (`down` only disconnects); `lock disable`
  needs the recovery secret and weakens tailnet security; `set --reset`/`up --reset` revert unspecified flags to
  defaults.
- **Privilege**: most `set`/`up` invocations need root on Linux, or the user set via `tailscale set --operator=$USER`.
  On macOS/Windows the GUI client typically owns the daemon; CLI changes may need elevation.
- **Not installed**: if `tailscale` isn't on `$PATH`, don't fabricate output — say so, and fetch the CLI docs or
  point at [installation.md](installation.md).

## CLI location by platform

- **macOS (standalone)**: client **Settings > CLI integration > Install Now** (macOS 13+) installs
  `/usr/local/bin/tailscale`.
- **macOS (App Store)**: run `/Applications/Tailscale.app/Contents/MacOS/Tailscale <command>`; set
  `TAILSCALE_BE_CLI=1` in scripts to force CLI mode.
- **Linux / Windows**: on `$PATH` after installation.

## Connection & authentication

```bash
tailscale up --auth-key=tskey-auth-xxxxx  # Headless (servers, CI)
tailscale up --login-server=https://...   # Custom control server
tailscale switch --list       # List profiles (Fast User Switching)
tailscale switch <tailnet>    # Switch profile
```

`tailscale up` flags worth knowing:

- `--accept-routes` — accept subnet routes advertised by others (Linux)
- `--accept-dns` — accept tailnet DNS configuration (default true)
- `--hostname`, `--shields-up` (block all incoming), `--force-reauth`
- `--reset` — reset unspecified settings to default values
- `--advertise-tags` — request specific tags (must be pre-authorized)
- `--timeout` — maximum wait for login (default 0, wait forever)

## Status & diagnostics

```bash
tailscale status --peers=false  # Only this device
tailscale ip -4                 # This device's IPv4 (-6 for IPv6; `ip <host>` for a peer)
tailscale whois 100.64.1.2      # Owner, hostname, tags of a Tailscale IP
tailscale version --daemon      # Daemon version (may differ from the client on some platforms)
tailscale netcheck              # NAT mapping type, UPnP/NAT-PMP/PCP, preferred DERP, DERP latencies
tailscale ping <host>           # TSMP ping; shows direct vs DERP-relayed
tailscale ping --icmp <host>    # ICMP through the WireGuard tunnel (also --tsmp, --peerapi)
tailscale nc <host> <port>      # Netcat-like TCP test through Tailscale
tailscale dns status            # DNS configuration
tailscale dns query <name>      # Look up a name via Tailscale DNS
```

Connectivity flow: `status` (online? peers visible?) → `ping` (direct or DERP?) → `netcheck` (NAT type, UDP) →
`status --json`. A DERP-relayed `ping` usually means UDP is blocked end-to-end or one side is behind a strict NAT —
`netcheck` confirms which. Background: [connectivity.md](connectivity.md).

## Configuration (`tailscale set`)

`set` changes configuration without reconnecting; most `up` flags are accepted (`tailscale help set` for all).

```bash
tailscale set --ssh                          # Enable Tailscale SSH server
tailscale set --advertise-exit-node          # Advertise as exit node
tailscale set --exit-node=<ip-or-hostname>   # Use a specific exit node
tailscale set --exit-node=                   # Stop using exit node
tailscale set --exit-node-allow-lan-access   # Allow LAN access while using exit node
tailscale set --advertise-routes=10.0.0.0/24 # Advertise subnet routes
tailscale set --accept-routes                # Accept routes from others (Linux)
tailscale set --operator=$USER               # Allow non-root user to manage
tailscale set --auto-update                  # Enable auto-updates
tailscale set --webclient                    # Enable web client interface
tailscale set --advertise-connector          # Advertise as an app connector
```

## Serve & Funnel

```bash
tailscale serve --bg 3000                                 # Proxy local :3000 over HTTPS :443 (--bg persists)
tailscale serve --bg --set-path=/docs /path/to/files      # Serve a directory under /docs
tailscale serve --bg --set-path=/health text:OK           # Static text
tailscale serve --bg --tcp=5432 tcp://localhost:5432      # Raw TCP forwarding
tailscale serve --bg --tls-terminated-tcp=5432 tcp://localhost:5432
tailscale serve https+insecure://localhost:8443           # Upstream with a self-signed cert
tailscale serve status
tailscale serve --https=443 --set-path=/docs off          # Remove a handler
tailscale serve reset                                     # Reset all serve config

tailscale funnel --bg 3000                                # Same, but public
tailscale funnel status
tailscale funnel reset
```

Serve provisions a TLS cert for the device FQDN (`machine.tailnet-name.ts.net`). Funnel requirements, ports and
limits: [sharing-and-publishing.md](sharing-and-publishing.md).

## Files

```bash
tailscale file cp photo.jpg my-laptop:   # Taildrop send — note the trailing colon
tailscale file get /path/to/download/dir # Receive (waits for incoming transfers)

tailscale drive share docs /home/user/Documents  # Taildrive share
tailscale drive list
tailscale drive rename docs documents
tailscale drive unshare docs
```

Taildrive shares appear at `\\machine\tailscale\share-name` (Windows) or via WebDAV.

## Security & administration

- `tailscale lock ...` — Tailnet Lock; subcommands and constraints in [connectivity.md](connectivity.md).
- `tailscale cert machine.tailnet-name.ts.net` — writes `.crt` and `.key` for the device FQDN; auto-renewed.
- `tailscale update [--check | --yes | --track=stable|unstable]`.
- `tailscale bugreport` — prints a bug report ID for support.
- `tailscale configure kubeconfig <hostname>` / `tailscale configure synology`.
- `tailscale metrics print` prints Prometheus text; `tailscale metrics write <path>` writes it to a file.
- `tailscale syspolicy list` / `reload` — MDM-managed policy values.
- `tailscale completion bash|zsh|fish|powershell` — e.g. `tailscale completion zsh > "${fpath[1]}/_tailscale"`.
