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
| Restart the proxy only when its binary changed (what `make install` runs) | `claude-code network restart --if-changed` (`rules/harness/providers.md`) |
| Run any harness verb on a tailnet host | `claude-code --host <host> <verb …>` (ssh, `-t` on a terminal); `databricks login` forwards its callback port so the sign-in happens in your local browser, or `--host-browser` opens the host's (`rules/harness/providers.md` → Headless login) |
| Run an ask here on the least busy Databricks workspace, requests through the other machine | `claude-code agents ask --agent <a> --overlay '{"provider":"databricks","profile":"auto","remote":"auto"}' --tools "<task>"` (what `/coworker` sends: `rules/workflows/agents.md` → Slash commands); `--host auto` instead runs the whole ask on that machine |
| Send Databricks requests out through a host | `claude-code --remote <host> <verb …>`: the verb runs here, workspace HTTPS leaves via `ssh -N -D` on `<host>`; `network restart` stops the tunnels |

`--remote` keeps this machine's config, vault and code and sets `HARNESS_EGRESS_HOST` for the verb and its children.
Requests to `*.cloud.databricks.com`, `*.azuredatabricks.net` and `*.gcp.databricks.com` go through one ssh SOCKS5
tunnel per host (record in `state/harness/ssh/`); agents reach the proxy at `/via/<host>/…`, so the daemon tunnels
per request and logs `via=<host>`. A dispatched agent's base also carries `/d/<id>/` (either order with `/via/`; id of
digits, letters, `.`, `-`, else 400), logged as `dispatch=<id>`; a stream cut mid-body logs `stream=broken:<class>`
(`eof`, `reset`, `timeout`, `canceled`, `other`). A tunnel that cannot start or has died fails the request with
`egress host <host>: <cause>`, never a direct connection. `--direct` is refused under `--remote`; a credential minted
by a provider's `auth.command` is that command's own traffic and is not tunnelled.

`--host auto` (`agents ask`, and the global flag) and `--remote auto` become a host name on this machine, before any
ssh call: the env value `HARNESS_REMOTE_HOST` (process env, else the vault's: `claude-code configure` →
environment), else the one other online tailnet machine that answers `claude-code version` over ssh within 5 s, kept
10 minutes in `state/harness/remote/auto-host.json`. No machine or more than one is an error that lists them; it
never guesses.

An overlay `profile: auto` picks, among the Databricks serving rows that pass the local checks (a stored credential, no
auth mark, and the model served to that agent, dropped when no workspace serves it; no token is requested, so a revoked
login or an unreachable workspace is only found on the first request), the workspace not cooling from a rate limit with
the fewest live dispatches (in-flight markers on its account plus running dispatch records routed to it); a tie goes to
the one picked least recently (`state/harness/ask/route-auto.json`), then name order. From there it is the overlay
`profile: <that workspace>`: the `ran on` line and the dispatch record name it, and with `--host` the pick is made here
and the concrete route is sent. Never name a profile `auto`: the overlay reads `profile: auto` as the pick, so such a
row cannot be addressed. When the picked workspace answers 429 before any answer, it is cooled, the pick reruns without
it and the same agent retries there, until one answers or none is left; only then the `--fallback` policy applies (so
`none` still fails, naming every workspace tried), and a named profile never moves. The retry serves a foreground ask
and a `--job` or `--background` run alike; `--session`, `--live` and `--host` never move. A `--host` ask that cannot run fails
before the task with one line naming what the target lacks: the `claude-code` binary, the agent's CLI, the Databricks
profile, or the model.

`--host <h>` or `--remote <h>` naming this machine (its tailnet DNS name, OS hostname, or either one's first label) runs
the verb here with no ssh hop and no tunnel, so one command line works on both machines.

- Browser automation only on authorized sites; robots.txt is checked; no detection evasion
  (`rules/standards/security.md` → 5).
- `doctor --mcp` is read-only: propose the fix (`claude mcp remove <name> -s <scope>`, or move a token to `${ENV}`
  via `vault set`); never remove or rotate without the user's confirmation.
- `clean` applies by default (`--dry-run` previews) and never sweeps credentials or settings.
