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
| Run an ask here on the least busy Databricks workspace, requests through the other machine | `claude-code --host auto agents ask --agent <a> --route databricks:auto --tools "<task>"` (what `/coworker` sends: `rules/workflows/agents.md` → Slash commands); `--remote auto` instead runs the whole ask on that machine |
| Send Databricks requests out through a host | `claude-code --host <host> <verb …>`: the verb runs here, workspace HTTPS leaves via `ssh -N -D` on `<host>`; `network restart` stops the tunnels |

`--host` keeps this machine's config, vault and code and sets `HARNESS_EGRESS_HOST` for the verb and its children.
Requests to `*.cloud.databricks.com`, `*.azuredatabricks.net` and `*.gcp.databricks.com` go through one ssh SOCKS5
tunnel per host (record in `state/harness/ssh/`); agents reach the proxy at `/via/<host>/…`, so the daemon tunnels
per request and logs `via=<host>`. A tunnel that cannot start or has died fails the request with
`egress host <host>: <cause>`, never a direct connection. `--direct` is refused under `--host`; a credential minted
by a provider's `auth.command` is that command's own traffic and is not tunnelled.

`--remote auto` (`agents ask`, and the global flag) becomes a host name on this machine, before any ssh call: the
env value `HARNESS_REMOTE_HOST` (process env, else the vault's: `claude-code vault edit` → `env`), else the one other
online tailnet machine that answers `claude-code version` over ssh within 5 s, kept 10 minutes in
`state/harness/remote/auto-host.json`. No machine or more than one is an error that lists them; it never guesses.

`--route databricks:auto` picks, among the Databricks serving rows whose credential resolves here, the workspace with
the fewest live dispatches (in-flight markers on its account plus running dispatch records routed to it; cap 24 per
account); a tie goes to the one picked least recently (`state/harness/ask/route-auto.json`), then name order. From
there it is `--route databricks:<that workspace>`: the `ran on` line and the dispatch record name it, and with
`--remote` the pick is made here and the concrete route is sent. No profile may be named `auto`. A `--remote` ask
that cannot run fails before the task with one line naming what the target lacks: the `claude-code` binary, the
agent's CLI, the Databricks profile, or the model.

- Browser automation only on authorized sites; robots.txt is checked; no detection evasion
  (`rules/standards/security.md` → 5).
- `doctor --mcp` is read-only: propose the fix (`claude mcp remove <name> -s <scope>`, or move a token to `${ENV}`
  via `vault set`); never remove or rotate without the user's confirmation.
- `clean` applies by default (`--dry-run` previews) and never sweeps credentials or settings.
