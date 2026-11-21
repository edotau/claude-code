---
paths:
  - "internal/providers/**"
  - "internal/vault/**"
  - "internal/models/**"
  - "internal/proxy/**"
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

- **One interface:** flags (`--profile`, `--model`, `--as`, `--fallback`, `--host`, `--remote`),
  `COWORKER_OVERLAY`, waves, coworker specs, `Run:` lines, `configure` menus and agent
  records all normalise into one overlay whose route is one unit, taken whole from one source;
  launch, preflight, labels and `ran on` read only it. Print it:
  (with no `--model`/`--profile` it prints the route ladder's first hop, the model the run and its label use)
  `claude-code gateway resolve <agent> --model m [--provider P] [--profile p] [--overlay '{...}']
  [--all-profiles [--live]] --format json`. A typed model the workspace's fresh listing lacks is refused (exit 2,
  nearest ids named) under any `--fallback`; with no fresh listing it says `unverified`.
- **Pool table:** `claude-code gateway resolve all --model auto --all-profiles [--task <brief>] [--format json]` peeks
  every rotation row x agent x tier (pool, next, held, last pick) without moving any rotation history; claude rotates
  no rows (its `auto` runs the brief's tier model on its one profile), so it has none here.
- **Model names (the one rule):** a model string is only ever a plain model id or alias (or `auto`); the
  profile/provider travels as the route: `--overlay '{"provider":"databricks","profile":"qa"}' --model opus`,
  `--profile`, `--provider` or the agent record. A prefixed value such as `dev:opus` is refused at every
  input, and the refusal names that overlay form. `auto` rotates models only, never the route.
  Every Claude alias resolves on the session's own profile: a cross-profile alias pin is ignored with a note.
- **Profile order** (the route, taken whole from the first source naming any of it; `--profile R` takes
  provider from R's registry row, never the session; `gateway resolve --provider databricks`, `--profile auto` too):
  `--profile` or overlay `profile` > overlay
  `provider`, then `account` > the session profile (with its model for the agent) > the agent record's profile or
  account (the pin) > the registry default > the agent's own sign-in row. Overlay
  `{"provider":"databricks","profile":"auto"}` or a pool pin picks the pool before these (first valid workspace in
  vault order dev, prod, qa; least-busy under `auto`); a pin on the agent's own sign-in row (copilot's GitHub row,
  gemini's Google row) skips the session.
- `<login>/auto` is refused before launch with that login's typed ids named.
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
- Any endpoint the workspace serves runs by its bare name (`gpt-6-1-terra` → `databricks-gpt-6-1-terra`); a typed
  unlisted or runtime-gone model is refused (exit 2) under any `--fallback`; a GitHub login's typed model is checked
  against its Copilot catalog snapshot.
- `opus` architecture and max-reasoning research; `sonnet` planning, review, orchestration, exploration (an untyped
  `claude` leaf, or one asked `auto`, gets sonnet for a standard-tier brief, haiku for fast, opus for deep; `auto`
  rotates models only, so the row stays the one its route names, the model following the tier as that row spells
  it); `haiku`
  high-frequency cheap
  leaves; `gemini` sweeps beyond a session window (1M tokens).

## Served-aware Databricks routing

`configure routing` → `servedAware` (off by default) checks codex, copilot and opencode picks against the workspace's
cached endpoint listing (`state/harness/preflight/served-<profile>.json`, fixed 24h TTL; a GitHub login's auto pool
ages by its Copilot snapshot instead, `HARNESS_MODEL_CACHE_TTL`, default 24h).
Preflight reuses credential-bound successes in router state for 15 minutes (failures and unknowns expire after
30 seconds and never block a launch); a token must cover the run timeout plus 2 minutes after keepalive or a
service-principal mint, unless one of those just renewed it (then a note, not a refusal).
Off, cold or stale reads as the static menu for ordinary picks; routing never touches the network.
Asked auto uses the served listing (a GitHub login: its Copilot catalog snapshot) alone for its pool: cold or stale
runs the row's tier model with a note; a fresh empty pool or unwritable pick state errors without launching
(details: `agent.md` → Model precedence).

- **Listing facts:** the cache also keeps each endpoint's `api_types`, `model_class` and the context and output limits
  parsed from its description. A fresh listing's `api_types` decide "serves Responses" for codex and `ResponsesServable`
  (Grok stays out of codex; opencode's routing is unchanged); a catalog limit beats the listing's; no listing, old rule.
- **Refresh:** `make install`, `doctor --refresh`, `models preflight --live`; switched on, also `switch`,
  `<agent> profile`, `configure` exit and a detached `models preflight --refresh`
  at session start (≤ once per 15 minutes). The detached catch-up child spawned at every session start also runs the
  daily learn steps (`models learn` fold, recall weights), stamped `state/harness/preflight/learn-daily`.
- **A swap ignores the alias:** an alias-derived pick (no `--model`, overlay `model` or record model) that is not
  READY is replaced by the first READY entry in menu order, so a sonnet dispatch can land on an opus endpoint; menu
  order is the control. A pinned or explicit model that is not READY is never swapped: its candidate is skipped
  (`<m> is not served on <p>` in `gateway` skips).
- **Live rescue:** a codex `ENDPOINT_NOT_FOUND` marks the model unserved for 1h and retries once on the next READY
  Responses model; an exact `--model` never retries. Auto picks also recover from model or capability refusals,
  sharing at most 3 re-picks per ask, never after streamed text or tools. Preflight refuses an unlisted name
  (a cold listing skips the check with a note).
- An OpenCode own-plan usage limit fails in seconds as `quota`, holding `opencode·go` until its reported reset.

## Databricks routes (key rule)

- Task-first how-to (failed call, add a model, throttling, cost): `skills/dbx-expert/SKILL.md`.

- Every agent uses the workspace serving endpoints, never a gateway product (an `options.wire` other than `serving` is
  refused): claude `<workspace>/serving-endpoints/anthropic`, codex `/responses`; copilot and opencode pick per model
  (Claude `/anthropic`, Responses-only GPTs `/responses`, the rest `/chat/completions`). Probe a route with a real
  `claude-code agents ask`, not `curl`.
- The `--verify` and `databricks check` probes send a streamed request and need the wire's terminator (a cut stream
  is `broken`, never auto-dropped); `claude-code databricks api <path>` is the read-only GET of allow-listed metadata
  paths (serving-endpoints, sql, scim Me, unity-catalog) with a Databricks profile's token, not `curl`. Detail:
  `providers-reference.md` → Probe verdicts, The `databricks api` verb.
- A model the proxy sees answered `400 This endpoint <id> is deprecated` is dropped at once, with its reason and
  date, and sends no extra call. The `--model auto` pool honours the drop list. A model Databricks later re-enables
  stays dropped until `claude-code models discover --undrop <id> --apply`.
- Cost: `claude-code usage --cost` is an estimate (local ledger tokens times the profile's price table), never an
  invoice. A billed report from `system.billing` is not available yet. Keys and grants: `providers-reference.md` →
  Cost keys.
- pi has one provider per wire on `http://127.0.0.1:18765/p/<profile>`: Claude over Anthropic Messages, GPT over
  Responses, everything else and Grok over chat completions. Its key is an environment variable the harness sets at
  launch to the proxy session token; no token is on disk. Pinned Pi packages (`packages` in the Pi record) add
  extension code that runs inside Pi with the user's privileges and is not judged by the guard; they never change a
  route. Rules and risk: `/Users/edotau/.claude/rules/harness/agent.md` → pi packages.
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
- On the Google route `gateway resolve`, `agents label` and `usage` read `agy -p /usage` (cached 10 min, spends no
  quota): the note carries the pool's percent left, under 15% warns, and a model at 0% is held until its reset.
- VS Code chat: the Claude Code Agents extension registers a "Claude Code Databricks" chat provider; its models come
  from `claude-code configure --vscode --print` and it fetches a session token with `claude-code resolve --proxy`,
  so no token sits in any file. `configure --vscode` removes the `Databricks <profile>` groups an older run wrote
  into `chatLanguageModels.json` (loopback `/p/<profile>/` URLs and `${input:harnessProxyToken}`); other groups stay.

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
- **Token states:** a cached Databricks token is fresh until expiry minus min(lifetime/2, 20 minutes) (10 minutes when
  the lifetime is unknown). Inside that window the proxy serves the current token at once and runs one background
  refresh per profile; from expiry minus a skew (1 minute, at most half the window) callers wait for an in-line
  refresh. A failed background refresh cools down for 1 minute and the valid token keeps being served. A short-lived
  command refreshes in line at the same early window.
- **A sent exchange finishes:** a u2m refresh token is single-use, so once an exchange is sent it always runs to its
  vault write. Callers and a proxy stop cannot cancel it; proxy stop, CLI exit and the launcher's exec wait up to
  2 minutes 15 seconds for it. If the vault write fails the call reports the rotation as unsaved (`seal_failed`).
  A process killed between send and save loses that login: renew it with `claude-code databricks login --profile <p>`.
  The proxy loads new token code only on a restart: `claude-code gateway proxy --restart`.
- **Throttling:** a service-principal mint (429, 5xx) and endpoint listing (429, 502, 503, 504) try 3 times, honour
  `Retry-After` (waits capped at 8 s) and otherwise back off from 500 ms with full jitter, capped at 4 s. A u2m
  refresh or login code is single-use: it is resent only when the request was never written, never after any HTTP
  reply. Model traffic uses the proxy's cool-down instead (`Retry-After`, 30 s default, 5 minutes at most).
- **One re-mint per rejection:** a 401/403 refresh adopts a token a peer minted after the rejected one instead of
  minting again (in-process under the flight lock; across processes via the sealed vault copy).
- Doctor's credential rows end with `source=vault`, `source=env <NAME>` or `source=command`.
- Login gotcha: `--remote <host> databricks login` always signs the user in (a paired SP does not mint) and seals the
  token here; `--host` forwards the callback port. A browser login with none of `--remote`, `--no-browser` or
  `--paste` takes the first `CLAUDE_REMOTE_HOST` entry as `<host>` (not on that host itself). Flags:
  `claude-code databricks login -h`.
