---
paths:
  - "internal/providers/**"
  - "internal/vault/**"
  - "internal/models/**"
  - "internal/gateway/**"
  - "internal/router/**"
  - "internal/agent/route*.go"
---
# Providers, Models, Credentials

| To… | Run |
| --- | --- |
| Check binaries, providers, credentials, proxy | `claude-code doctor` · leaked credentials: `doctor --scan\|--git\|--secrets` |
| See providers and credential state | `claude-code configure provider --list [<name>]` |
| Set a credential | `claude-code vault set [NAME]` (bare on a terminal = guided dialog), or a provider's `auth.command` |
| Inspect or edit the vault | `claude-code vault [ls]\|get\|delete\|import\|export\|edit\|rotate-secret` |
| Switch the session profile | `claude-code switch <profile> [--dry-run]` (relaunch) · one launch: `claude-code --profile <p>`; with no flag, an interactive claude, copilot or opencode launch reuses the profile that agent last launched in this repo (if still valid for it), else its agent record's profile (a default; `state/harness/last-profile.json`; `switch` clears all, `<agent> profile` that agent) |
| Set claude's models | `claude-code models --pin opus=<id>` · `--pin all=<id>` · `--unpin` write claude's agent record (`--dry-run` previews) |
| List what a provider serves | `claude-code models --live [--profile <p>]` |
| Run the model proxy | `claude-code gateway proxy --start\|--stop\|--status` · foreground: `--serve --port N` |
| Proxy restart and watchdog | `gateway proxy --restart` gaps only the respawn time (the old daemon drains in the background); a routed session's status line restarts a down proxy within one refresh (claims ≥15s apart) |

- Secrets only in the vault or `auth.command` (CLAUDE.md hard rule 2); `configure provider --list` shows the
  source, never the value.
- Agent settings are vault-only, one record per agent (`vault` AGENT table); `--model` beats them.
  `switch` writes claude's record too.
- Built-in providers are `internal/providers/defaults.json`; the vault's provider overlay replaces one by name.
- Agents run on their pins (gemini on Google); the all-Databricks arm is a launch flag only: `--profile dev|qa|prod` /
  `--provider databricks` on a dispatch, `profile`/`provider` on a workflow call. Nothing stores an arm default.

## Model selection

Name the alias (`opus`/`sonnet`/`haiku`/`fable`), never a concrete model id: `internal/providers/defaults.json` plus
the vault overlay map each alias per provider.

- **`opus`** — architectural decisions, maximum-reasoning research and analysis.
- **`sonnet`** — planning, review, orchestration, exploration; the default `claude` leaf.
- **`haiku`** — high-frequency, cost-sensitive leaves and quick lookups.
- **Gemini** (`claude-code agents ask --agent gemini --tools --dirs <d>`) — sweeps whose context exceeds a session
  window (1M tokens).

## Served-aware Databricks routing

`configure routing` → `servedAware` (off by default) checks codex, copilot and opencode picks against the workspace's
cached endpoint listing (`state/harness/preflight/served-<profile>.json`, 24h TTL). Off, cold or stale reads as today's
static menu: routing never touches the network.

- **Refresh:** `make install`, `doctor --refresh`, `preflight --live`; with the switch on, also `switch`,
  `<agent> profile`, `configure` exit (record changed or switch flipped on) and a detached `preflight --refresh` at
  session start, at most once per 15 minutes.
- **Fallback ignores the model alias:** a swap takes the first READY entry in menu order, so a sonnet dispatch can land
  on
  an opus endpoint. Menu order is the control. An explicit model in the record swaps too; the swap appears in
  `gateway` skips.
- **Live rescue:** a codex dispatch that gets `ENDPOINT_NOT_FOUND` marks the model unserved for 1h and retries once
  on the next READY Responses model. An exact `--model` never retries. copilot and opencode refuse unknown models
  client-side, so there is no captured failure text for them.

## Databricks routes (key rule)

- Every agent uses the workspace serving endpoints, never AI Gateway (`/ai-gateway/*`): claude
  `<workspace>/serving-endpoints/anthropic`, codex `/serving-endpoints/responses` (stays on Responses), copilot and
  opencode `/serving-endpoints/chat/completions`. Probe a route with a real `claude-code agents ask`, not a hand-built
  `curl`.

## Databricks service-principal identity

- A u2m (`oauth`) row may pair a service principal: `auth.spClientId` (not secret) plus the sealed secret
  `DATABRICKS_<PROFILE>_M2M`; `identity` is `auto` (default) | `user` | `sp`. Unpaired rows are unchanged.
- Precedence: `--as` (carried by `providers.WithIdentity(ctx, as)`) > the row's `identity` > `auto`.
- Headless dispatch (`agents ask`, `--batch`, `--background`/job, `--live`, `--host`) with no `--as` runs a paired
  row left at `auto` as its SP first (`IdentityHeadless`), falling back to the user login with one stderr note when
  the SP secret is missing or its mint is rejected; `--as auto|user` overrides; the proxy gets it via
  `X-Claude-Code-Identity`. No credential path opens a browser: only `databricks login` (not on an `sp` row, which
  mints) and an interactive prompt do.
- `auto` tries the user token, then `oauth.MintM2M` on a rejection or login-required, with a one-line stderr note;
  `user` never mints the SP; `sp` mints only the SP, no browser. The SP never refreshes the user login.

## Headless login

`databricks login` (a u2m row; not `--as sp`, which rejects these flags) works without a local browser:

- Over SSH: `ssh -L 8020:localhost:8020 <host>`, then run `claude-code databricks login --no-browser --port 8020` on
  the host and open the printed URL in your own browser.
- `--port N` listens on exactly N and fails if another process holds it; without it the callback walks 8020 to 8040.
- `--no-browser` opens nothing and always prints the URL and the `ssh -L` hint (not only when `SSH_CONNECTION` is set).
- `--paste` also reads one line from stdin: with no forward, copy the failed `http://localhost:<port>/?code=...` URL
  from the browser's address bar and paste it. Its `state` is checked; the callback and the paste race, first wins.
- `--dry-run` with these flags prints the port and modes and opens nothing.
