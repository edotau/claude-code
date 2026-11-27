---
paths:
  - "internal/remote/network/**"
  - "internal/platform/clipboard/**"
  - "internal/dev/setup/**"
---
# Network, Web, MCP, Cleanup

- Web: `claude-code network websearch --query "<query>"` or `--extract <url>`; `skills/web-explorer/SKILL.md`.
- Browser setup: `claude-code network browser [--setup --browser <b>]`.
- MCP: `claude-code doctor --mcp [--format json]` is read-only. Propose removal/scope fixes or moving tokens
  to vault-backed `${ENV}`; never remove/rotate without owner confirmation.
- Cleanup: `claude-code clean [--list|--dry-run]` applies by default, never sweeps credentials/settings.
  Projections/sessions require opt-in `--target`.
- Proxy: `claude-code gateway proxy --status` lists standing scopes; `--stop` stops them and the daemon.
  `--restart --if-changed` restarts only after binary changes (install/status line run it when idle).
  Actual changed-binary restarts stop tunnels; plain restart leaves them. In-flight requests need `--force`.
- `claude-code --host <h> <verb …>` runs remotely over SSH, with TTY on a terminal.
  Databricks login forwards the callback or uses `--host-browser`.
  `--remote <h>` runs here but sends Databricks HTTPS through that host's SOCKS tunnel.

## Corporate CA (`network pki`)

`claude-code network pki` writes `state/certs/corp-ca-bundle.pem`, root.pem and issuing.pem; `--trust` reads there.
When unset/empty, SSL_CERT_FILE, REQUESTS_CA_BUNDLE, CURL_CA_BUNDLE and NODE_EXTRA_CA_CERTS use the bundle;
non-empty owner values win. Overlays refuse SSL_CERT_FILE, SSL_CERT_DIR and NODE_EXTRA_CA_CERTS.
The install clone copies an old XDG cert bundle once, never deletes it.
Doctor reports age/earliest expiry (fails within 30 days)/exports. For another user's node service, set `--out`
to a readable/traversable path. Other managed certs use `state/certs/<area>/` unless out/trust overrides.

- `--remote` keeps this machine's config, vault and code and sets `HARNESS_EGRESS_HOST`. Requests to
  Databricks cloud/azure/gcp domains use one SSH SOCKS5 tunnel per host (`state/harness/ssh/`);
  reuse only a pid verified as our SSH tunnel on that port. Agents use `/via/<host>/…`.
  Failed/dead tunnels fail requests, never connect directly. Remote refuses `--direct`;
  provider `auth.command` credential minting is not tunneled.
- Standing scoped proxies use free loopback ports; stop/status/doctor remove dead scope records.
  Phase proxies live only for their wave/batch and are not listed as standing.
- Auto host/remote: process then vault `CLAUDE_REMOTE_HOST` (first comma-separated entry), else the sole online
  tailnet peer answering `claude-code version` within 5 seconds (10-minute auto-host.json cache).
  Zero/multiple peers fail. This machine's DNS/hostname/first label runs locally without SSH/tunnel.
- Auto profile chooses locally eligible Databricks rows (credential, no auth mark, model served), with no token probe:
  revoked logins fail on request. Exclude rate-limit cooldowns; choose fewest dispatches, least recently picked, name.
  Never name a profile auto. Batch chooses per line, counting prior picks as load.
  Unserved typed models fail with up to five served ids unless explicit fallback pin/agents/all permits swapping.
- A 429 before any answer cools the row and tries other workspaces before fallback; none fails with tried workspaces.
  Named profiles, explicit batch-line profiles, session/live/background launches and host asks stay pinned;
  unpinned batch lines and background job children may move.
- Remote asks preflight target binary, agent CLI, Databricks profile and model before running the task.
- Claude web tools use Tavily via launch hooks; Databricks proxy handles search already, so only fetch is replaced.
  Hook details: `rules/harness/hooks.md` → Event → hook matrix.

## Clipboard over SSH

Clipboard paste reads the SSH client's Mac clipboard through a forwarded socket, never the WSL host's.
Owner setup:

```text
# Mac ~/.ssh/config
Host <wsl-host>
    RemoteForward /tmp/claude-clip-%r.sock %d/.claude/run/clip.sock

# WSL /etc/ssh/sshd_config (restart ssh after changing)
StreamLocalBindUnlink yes
```

Mac install provides on-demand launchd `clip --serve`; no per-session process.
The bridge serves only Desktop, Downloads, Documents, Pictures, Movies, Music and temp roots.
Run `claude-code clip` (or the `wl-paste`/`clip paste` shim) to save the image and print its path.
Check forwarding, socket type/ownership and a copied image on failure; another user's socket is refused.
