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
| Inspect or edit the vault | `claude-code vault [ls]\|get\|delete\|import\|export\|rotate-secret` (`get` redacts; `--secret` prints, main session only); env and overlays: `claude-code configure` |
| Switch the session profile | `claude-code switch <profile> [--dry-run]` · one launch: `claude-code --profile <p>` |
| Set claude's models | `claude-code models --pin opus=<id>` · `--pin all=<id>` · `--unpin` (`--dry-run` previews) |
| List what a provider serves | `claude-code models --live [--profile <p>]` |
| Run the model proxy | `claude-code gateway proxy --start\|--stop\|--status\|--restart` · foreground: `--serve --port N` |

- Transcript cleanup: `claude-code doctor --secrets --fix` masks idle Claude and harness transcript stores in place at
  the same byte length; optional paths restrict the pass. Codex and Copilot session stores are not enabled yet.
  `--secrets` scans transcripts modified within seven days (`--days N` changes the window, `--days 0` covers all
  ages) and prints each store's counts and older-file skips; `--since N` limits git commits only.
- Secrets: CLAUDE.md hard rule 2; `configure provider --list` shows the source, never the value. Credential verbs
  (`vault get`, `resolve`, `gateway proxy --client-token`, `tailscale --token`) print `<redacted: N bytes>`
  (to a pipe also exit 1) unless `--secret`, which only the main session may run; a leaf gets credentials at launch
  (vault env `@secret`, or a dispatch overlay `set` of `@secret:<NAME>`). Built-in providers are
  `internal/providers/defaults.json`; a vault overlay row replaces one by name.
- Agent settings live in the vault, one record per agent (`vault` AGENT table); an external agent's shell
  `<AGENT>_MODEL` outranks the record's `model`, and `--model` beats both; `switch` and `<agent> profile` write the
  record (no last-used profile exists).
- Agents run on their pins (gemini on Google); per-call routes use the overlay: keys, precedence and fallback in
  `agent.md` → Dispatch behavior. A standing default for top-level dispatches is vault env `COWORKER_OVERLAY`
  (`claude-code vault env set\|unset COWORKER_OVERLAY`). Brief fields: `skills/subagent-workflows/SKILL.md`.
- `gateway proxy --restart` gaps only the respawn; a routed session's status line restarts a down proxy; `make install`
  never restarts it, only notes when the running proxy is an older build (`proxy.json` `exe` sha256).

## Routing precedence

- **One interface:** flags (`--profile`, `--model`, `--as`, `--fallback`, `--host`, `--remote`), `<profile>:` and
  provider-type model prefixes, `COWORKER_OVERLAY`, waves, coworker specs, `Run:` lines, `configure` menus and agent
  records all normalise into one overlay; launch, preflight, labels and `ran on` read only it. Print it:
  (with no `--model`/`--profile` it prints the route ladder's first hop, the model the run and its label use)
  `claude-code gateway resolve <agent> --model m [--profile p] [--overlay '{...}']
  [--all-profiles [--live]] --format json`. A model the workspace's fresh listing lacks is marked `served: no` with
  the served alternatives named, and `--fallback none` exits non-zero instead; with no fresh listing it says
  `unverified`.
- **Profile order:** a `<profile>:` or unique provider-type model prefix > `--profile` or overlay `profile` > overlay
  `provider`, then `account` > the session profile (with its model for the agent) > the agent record's profile or
  account (the pin) > the registry default > the agent's own sign-in row. `databricks:<m>` or profile `auto` picks the
  pool before these (first valid workspace in vault order dev, prod, qa; least-busy under `auto`); a pin on the
  agent's own sign-in row (copilot's GitHub row, gemini's Google row) skips the session.
- **`--profile` vs a prefix:** an interactive launch errors naming both; `agents ask` and dispatches let the prefix win
  with a note, but an overlay confinement refuses a prefix outside it.
- **Spelling:** Databricks rows `databricks-<canonical>` (a bare id gains the prefix only when the workspace lists it);
  first-party Anthropic bare; GitHub, ChatGPT and agy dotted.
  Claude 5+ is 1M by default; `[1m]` only for opus/sonnet 4.x.
- **Fallback** (`AGENT_ERROR_FALLBACK`, `--fallback`, overlay `fallback`): `none` | `pin` | `agents` | `all`
  (`1`/`true`/`yes` = `all`; unset = `none`). `pin` = requested route > agent pin > record `fallback` > stop. Set it in
  `claude-code configure` or `claude-code configure agents <agent> profile=<p> model=<m> fallback=<p>` (empty clears).
- **Labels:** `ran on` names what served, each fallback hop included; dispatch labels and the session-start per-type
  labels come from the same resolver.

## Model selection

Name the alias (`opus`/`sonnet`/`haiku`/`fable`), never a concrete model id: `internal/providers/defaults.json` plus
the vault overlay map each alias per provider.

- On a Databricks profile an alias picks the first member of its bucket (`internal/router/table.json` → `databricks`)
  the agent can run and, with a fresh listing, the workspace lists READY. An explicit `--model`, overlay `model` and
  record model outrank the bucket.
- A GPT family name (`sol`, `astra`, `terra`, `luna`) is the highest numeric version the row's listing serves READY
  (`6.1` = `6-1` < `6.2` < `7`); no listing or member falls back to today's id. A concrete id stays exactly that id.
- Model `auto` is the harness pick, never a wire id: a listed model named auto is ignored.
- Any endpoint the workspace serves runs by its bare name (`gpt-6-1-terra` → `databricks-gpt-6-1-terra`); an unlisted
  name is refused at preflight (with `--fallback` it falls back instead).
- `opus` architecture and max-reasoning research; `sonnet` planning, review, orchestration, exploration (an untyped
  `claude` leaf gets sonnet for a standard-tier brief, haiku for fast, opus for deep); `haiku` high-frequency cheap
  leaves; `gemini` sweeps beyond a session window (1M tokens).

## Served-aware Databricks routing

`configure routing` → `servedAware` (off by default) checks codex, copilot and opencode picks against the workspace's
cached endpoint listing (`state/harness/preflight/served-<profile>.json`, 24h TTL).
Preflight reuses credential-bound successes in router state for 15 minutes (failures and unknowns expire after
30 seconds and never block a launch); a token must cover the run timeout plus 2 minutes after keepalive or a
service-principal mint, unless one of those just renewed it (then a note, not a refusal).
Off, cold or stale reads as the static menu: routing never touches the network.

- **Refresh:** `make install`, `doctor --refresh`, `models preflight --live`; switched on, also `switch`,
  `<agent> profile`, `configure` exit and a detached `models preflight --refresh`
  at session start (≤ once per 15 minutes). The detached catch-up child spawned at every session start also runs the
  daily learn steps (`models learn` fold, recall weights), stamped `state/harness/preflight/learn-daily`.
- **A swap ignores the alias:** an alias-derived pick (no `--model`, overlay `model` or record model) that is not
  READY is replaced by the first READY entry in menu order, so a sonnet dispatch can land on an opus endpoint; menu
  order is the control. A pinned or explicit model that is not READY is never swapped: its candidate is skipped
  (`<m> is not served on <p>` in `gateway` skips).
- **Live rescue:** a codex `ENDPOINT_NOT_FOUND` marks the model unserved for 1h and retries once on the next READY
  Responses model; an exact `--model` never retries. copilot and opencode have none: preflight refuses an unlisted name
  (a cold listing skips the check with a note).

## Databricks routes (key rule)

- Every agent uses the workspace serving endpoints, never a gateway product (an `options.wire` other than `serving` is
  refused): claude `<workspace>/serving-endpoints/anthropic`, codex `/responses`; copilot and opencode pick per model
  (Claude `/anthropic`, Responses-only GPTs `/responses`, the rest `/chat/completions`). Probe a route with a real
  `claude-code agents ask`, not `curl`.
- gemini (agy) speaks chat/completions through the proxy's `openai` passthrough, so on a Databricks profile it runs
  any chat model the workspace serves when one is typed with `--model` (or `--model auto`); with no model or a tier
  alias it still runs its Gemini pin. The Responses-only models (`gpt-6-1-sol`, `gpt-6-astra`, `gpt-6-sol`, `gpt-5-5`,
  `gpt-5-5-pro`) are refused for it; run them with codex, copilot or opencode.
- On its Google sign-in row agy's own models are typed as catalog id plus effort (`gemini-4-hydrogen-xhigh`,
  `claude-sonnet-5-5-medium`, `gpt-oss-120b`); accepted efforts live in `internal/models/catalog.json` `efforts`, and
  an unlisted one fails before agy launches. The profile in force picks the backend, never the model name.
  `--autonomous` is dropped on gemini (agy's `--mode auto` is account-gated).
- With no `--profile`, gemini sends a typed `databricks-*` model to the session's Databricks gateway row and a
  Gemini model to the Google sign-in row (unless a `GEMINI_PROFILE` gateway pin holds); an unprefixed `claude-*` or
  `gpt-oss-120b` follows the profile in force. agy signs in by running `agy` (there is no `agy auth login`).
- On the Google route `gateway resolve`, `agents label` and `usage` read `agy -p /usage` (cached 60 s, spends no
  quota): the note carries the pool's percent left, under 15% warns, and a model at 0% is held until its reset.
- VS Code chat: `claude-code configure theme vscode` merges one `Databricks <profile>` group per signed-in serving
  profile into `chatLanguageModels.json`, routed through `/p/<profile>/`; other groups stay byte-for-byte.
  The file holds only `${input:harnessProxyToken}`: mint it with
  `gateway proxy --client-token <name> --secret` (printed once; the vault
  keeps its SHA-256), revoke with `--revoke-client-token <name>`. The proxy accepts it only on Databricks serving `/p/`
  routes.

## Databricks service-principal identity

- A u2m (`oauth`) row may pair a service principal: `auth.spClientId` plus sealed secret `DATABRICKS_<PROFILE>_M2M`;
  `identity` is `auto` (default) | `user` | `sp` (`claude-code databricks identity --profile <p> auto|user|sp`).
  Precedence: `--as` > the row's `identity` > `auto`.
- `auto` serves the paired SP first when no user login is stored or a dead login is recorded; otherwise it tries the
  user token, then `oauth.MintM2M` on rejection, login-required or an unreadable vault, with a one-line stderr note;
  `user` never mints the SP; `sp` mints only the SP. The SP never refreshes the user login.
- Headless dispatch (`agents ask`, `--batch`, `--background`, `--live`, `--host`) with no `--as` runs a paired `auto`
  row as its SP first, falling back to the user login with a note when the secret is missing or the mint rejected.
  No credential path opens a browser except `databricks login` on a row with no stored SP secret (or `--as user`, or
  `--remote`) and an interactive prompt; on an `sp` row, or an `auto` row with a stored SP secret, `login` mints the
  SP.
- A dead user login (`needs_login`) is remembered in `state/harness/cache/login-state/<profile>.json` (state and times
  only) and reported: `doctor` and `databricks check --all` say the SP covers, the status line shows
  `login:<profile>`, a proxy 401 names `claude-code --remote <host> databricks login --profile <p>`. It is not
  re-probed on a timer; sealing a login (including `vault set`/`import` of a record with `refresh`) clears it.
- While the proxy runs it refreshes tokens of profiles used in the last 24 h once under 15 minutes remain, under the
  per-profile lock. A rotated refresh token is carried until sealed (failure audits `seal_failed`); a stale one never
  replaces a newer vault copy.
- Login gotcha: `--remote <host> databricks login` always signs the user in (a paired SP does not mint) and seals the
  token here; `--host` forwards the callback port. Flags: `claude-code databricks login -h`.
