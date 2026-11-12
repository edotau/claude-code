---
paths:
  - "internal/network/**"
  - "internal/setup/**"
---
# Network, Web, MCP, Cleanup

| To… | Run |
| --- | --- |
| Search the web or read a page (any agent) | `claude-code network websearch --query "<query>"` · `--extract <url>` (`skills/web-explorer/SKILL.md`) |
| Check MCP health, scope drift, inline secrets | `claude-code doctor --mcp [--format json]` |
| Sweep accumulated state (caches, run dirs, backups) | `claude-code clean [--list\|--dry-run]` · opt-in `--target projections\|sessions` |
| Run any harness verb on a tailnet host | `claude-code --remote <host> <verb …>` (ssh, `-t` on a terminal); `databricks login` forwards its callback port so the sign-in happens in your local browser, or `--remote-browser` opens the host's (`rules/harness/providers.md` → Headless login) |
| Send Databricks requests out through a host | `claude-code --host <host> <verb …>`: the verb runs here, workspace HTTPS leaves via `ssh -N -D` on `<host>`; `network restart` stops the tunnels |

`--host` keeps this machine's config, vault and code and sets `HARNESS_EGRESS_HOST` for the verb and its children.
Requests to `*.cloud.databricks.com`, `*.azuredatabricks.net` and `*.gcp.databricks.com` go through one ssh SOCKS5
tunnel per host (record in `state/harness/ssh/`); agents reach the proxy at `/via/<host>/…`, so the daemon tunnels
per request and logs `via=<host>`. A tunnel that cannot start or has died fails the request with
`egress host <host>: <cause>`, never a direct connection. `--direct` is refused under `--host`; a credential minted
by a provider's `auth.command` is that command's own traffic and is not tunnelled.

- Browser automation only on authorized sites; robots.txt is checked; no detection evasion
  (`rules/standards/security.md` → 5).
- `doctor --mcp` is read-only: propose the fix (`claude mcp remove <name> -s <scope>`, or move a token to `${ENV}`
  via `vault set`); never remove or rotate without the user's confirmation.
- `clean` applies by default (`--dry-run` previews) and never sweeps credentials or settings.
