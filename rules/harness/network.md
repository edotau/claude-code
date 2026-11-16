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
| Sweep accumulated state (caches, run dirs, backups, leftover `memory sandbox` dirs) | `claude-code clean [--list\|--dry-run]` · opt-in `--target projections\|sessions` |
| Restart the proxy only when its binary changed (what `make install` runs) | `claude-code network restart --if-changed` |
| Run any harness verb on a tailnet host | `claude-code --host <host> <verb …>` (ssh, `-t` on a terminal); `databricks login` forwards its callback port, or `--host-browser` opens the host's browser |
| Least-busy Databricks workspace, requests through the other machine | `claude-code agents ask --agent <a> --overlay '{"provider":"databricks","profile":"auto","remote":"auto"}' --tools "<task>"` (what `/coworker` sends); `--host auto` runs the whole ask on that machine |
| Send Databricks requests out through a host | `claude-code --remote <host> <verb …>`: the verb runs here, workspace HTTPS leaves via `ssh -N -D` on `<host>`; `network restart` stops the tunnels |

- `--remote` keeps this machine's config, vault and code and sets `HARNESS_EGRESS_HOST`. Requests to
  `*.cloud.databricks.com`, `*.azuredatabricks.net` and `*.gcp.databricks.com` go through one ssh SOCKS5 tunnel per host
  (record in `state/harness/ssh/`); agents reach the proxy at `/via/<host>/…`. A tunnel that cannot start or has died
  fails the request with `egress host <host>: <cause>`, never a direct connection. `--direct` is refused under
  `--remote`; a credential minted by a provider's `auth.command` is not tunnelled.
- `--host auto` and `--remote auto` resolve to a host name here, before any ssh call: `CLAUDE_REMOTE_HOST` (process
  env, else the vault's), else the one other online tailnet machine that answers `claude-code version` over ssh within
  5 s (cached 10 minutes in `state/harness/remote/auto-host.json`). None or several is an error listing them.
- `--host <h>` or `--remote <h>` naming this machine (tailnet DNS name, OS hostname, or either one's first label) runs
  the verb here with no ssh hop or tunnel.
- Overlay `profile: auto` picks, among Databricks serving rows passing the local checks (stored credential, no auth
  mark, model served to that agent; no token is requested, so a revoked login is found on the first request), the
  workspace not cooling from a rate limit with the fewest live dispatches; a tie goes to the least recently picked
  (`state/harness/ask/route-auto.json`), then name order. Never name a profile `auto`: it cannot be addressed.
- On a 429 before any answer, the pick is cooled and the agent retries on the next workspace until one answers or none
  is left; only then `--fallback` applies (`none` still fails, naming every workspace tried). A named profile,
  `--batch` lines, `--session`, `--live` and `--host` never move.
- A `--host` ask that cannot run fails before the task with one line naming what the target lacks: the `claude-code`
  binary, the agent's CLI, the Databricks profile or the model.
- `doctor --mcp` is read-only: propose the fix (`claude mcp remove <name> -s <scope>`, or move a token to `${ENV}` via
  `vault set`); never remove or rotate without the user's confirmation.
- `clean` applies by default (`--dry-run` previews) and never sweeps credentials or settings.
