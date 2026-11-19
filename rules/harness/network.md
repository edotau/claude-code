---
paths:
  - "internal/network/**"
  - "internal/clipboard/**"
  - "internal/setup/**"
---
# Network, Web, MCP, Cleanup

| To… | Run |
| --- | --- |
| Search the web or read a page (any agent) | `claude-code network websearch --query "<query>"` · `--extract <url>` (`skills/web-explorer/SKILL.md`) |
| Check MCP health, scope drift, inline secrets | `claude-code doctor --mcp [--format json]` |
| Sweep accumulated state (caches, run dirs, backups, stale launch overlays, leftover `memory sandbox` dirs) | `claude-code clean [--list\|--dry-run]` · opt-in `--target projections\|sessions` |
| Restart the proxy only when its binary changed (`make install` only notes it) | `claude-code gateway proxy --restart --if-changed` |
| Run any harness verb on a tailnet host | `claude-code --host <host> <verb …>` (ssh, `-t` on a terminal); `databricks login` forwards its callback port, or `--host-browser` opens the host's browser |
| Least-busy Databricks workspace, requests through the other machine | `claude-code agents ask --agent <a> --overlay '{"provider":"databricks","profile":"auto","remote":"auto"}' --tools "<task>"` (what `/coworker` sends); `--host auto` runs the whole ask on that machine |
| Send Databricks requests out through a host | `claude-code --remote <host> <verb …>`: the verb runs here, workspace HTTPS leaves via `ssh -N -D` on `<host>`; `gateway proxy --restart --if-changed` (add `--force` with requests in flight) stops the tunnels after an actual restart; plain `--restart` leaves them running |

- `--remote` keeps this machine's config, vault and code and sets `HARNESS_EGRESS_HOST`. Requests to
  `*.cloud.databricks.com`, `*.azuredatabricks.net` and `*.gcp.databricks.com` go through one ssh SOCKS5 tunnel per host
  (record in `state/harness/ssh/`; a recorded tunnel is reused only while its pid is still our `ssh -N -D` on that
  port, any other listener there gets a fresh tunnel); agents reach the proxy at `/via/<host>/…`.
  A tunnel that cannot start or has died fails the request with `egress host <host>: <cause>`, never a direct
  connection. `--direct` is refused under `--remote`; a credential minted by a provider's `auth.command` is not
  tunnelled.
- `--host auto` and `--remote auto` resolve to a host name here, before any ssh call: `CLAUDE_REMOTE_HOST` (process
  env, else the vault's; a comma-separated list, the first entry is the machine named), else the one other online
  tailnet machine that answers `claude-code version` over ssh within 5 s (cached 10 minutes in
  `state/harness/remote/auto-host.json`). None or several is an error listing them.
- `--host <h>` or `--remote <h>` naming this machine (tailnet DNS name, OS hostname, or either one's first label) runs
  the verb here with no ssh hop or tunnel.
- Memory sync is off unless this machine's process or vault env has `NETWORK_MEMORY=on`. When on, every 4 hours and
  only while the machine is in use, it connects over ssh to each host in `CLAUDE_REMOTE_HOST` and sends and fetches
  only the memory store's files. A machine that is only a peer never connects for sync. `claude-code doctor` shows the
  state on the `memory sync` rows.
- Overlay `profile: auto` picks, among Databricks serving rows passing the local checks (stored credential, no auth
  mark, model served to that agent; no token is requested, so a revoked login is found on the first request), the
  workspace not cooling from a rate limit with the fewest live dispatches; a tie goes to the least recently picked
  (`state/harness/router/route-auto.json`), then name order. Never name a profile `auto`: it cannot be addressed.
  `ask --provider databricks` with no `--profile` is the same pick; a `--batch` picks per line, each line counted as
  load for the next. A typed model no workspace serves fails before launch naming up to five served ids, unless
  `--fallback` was typed as `pin`, `agents` or `all` (an env default never allows the swap).
- On a 429 before any answer, the pick is cooled and the agent retries on the next workspace until one answers or none
  is left; only then `--fallback` applies (`none` still fails, naming every workspace tried). A named profile, a
  `--batch` line that names its own `profile`, `--session`, `--live`, a `--background` launch and `--host` never move;
  a `--batch` line without `profile` and the `--job` child of a `--background` run do.
- A `--host` ask that cannot run fails before the task with one line naming what the target lacks: the `claude-code`
  binary, the agent's CLI, the Databricks profile or the model.
- `doctor --mcp` is read-only: propose the fix (`claude mcp remove <name> -s <scope>`, or move a token to `${ENV}` via
  `vault set`); never remove or rotate without the user's confirmation.
- `clean` applies by default (`--dry-run` previews) and never sweeps credentials or settings.

## Clipboard over SSH

Image paste in an SSH session reads the client's clipboard, never the WSL host's: the `wl-paste` shim
(`claude-code clip paste`) and `claude-code clip` go through a forwarded socket (`/tmp/claude-clip-<user>.sock`) to a
Mac-side bridge. Two one-time owner steps:

```text
# Mac ~/.ssh/config
Host <wsl-host>
    RemoteForward /tmp/claude-clip-%r.sock %d/.claude/run/clip.sock

# WSL /etc/ssh/sshd_config (then: sudo systemctl restart ssh)
StreamLocalBindUnlink yes
```

`make install` on the Mac installs an on-demand launchd agent (`clip --serve` on `~/.claude/run/clip.sock`); nothing
runs per session. The Mac serves files only from `~/Desktop`, `~/Downloads`, `~/Documents`, `~/Pictures`, `~/Movies`,
`~/Music` and the temp roots (anything else: `not shareable: outside <roots>`). Diagnose from the SSH session with
`claude-code clip` (saves the image, prints its path). Failures:
`no clipboard bridge from the SSH client (<sock>: …)` (forward missing, not a
socket, or "is not owned by you" — a socket another user pre-bound is refused), and `no image in the SSH client's
clipboard — copy one there first (or its bridge is down)`.
