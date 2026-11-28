---
paths:
  - "internal/routing/providers/**"
  - "internal/routing/models/**"
  - "internal/routing/router/**"
  - "internal/routing/databricks/**"
  - "internal/routing/catalog/**"
  - "internal/platform/vault/**"
  - "internal/proxy/**"
  - "internal/agent/route*.go"
---
# Providers, Models, Credentials

Provider schema, credentials, route precedence, the proxy and Databricks specifics. Elsewhere: per-call overlay
and the top-level dispatch default (`agent.md` → Overlay); `auto`, rotation, tier fallback and refusal recovery
(`agent.md` → Model precedence); proxy route order (`agent.md` → Proxy routes); pi wires and packages (`agent.md` →
pi); task-first Databricks how-to (`skills/dbx-expert/SKILL.md`). Secrets: CLAUDE.md hard rule 2.

## Naming

- agent: the CLI doing the work: `claude` `codex` `copilot` `opencode` `gemini`.
- provider: the platform hosting the endpoint: `anthropic` `databricks` `github` `openai` `google` `magnitude`.
- profile: an account on a provider; carries billing; unique across providers, `[A-Za-z0-9_-]`; code never parses it.
- auth: `oauth` (user login), `m2m` (service principal), `pat`, `key`, `cli` (the agent's own login), `none`.
- model: what `--model` takes: an alias (`opus`), a version (`opus-5-5`), a full wire id, or a bare served endpoint
  name (`gpt-6-1-terra`). Ids are built per provider from the canonical form (`opus-5-5` → `claude-opus-5-5`,
  `databricks-claude-opus-5-5`, `system.ai.claude-opus-5-5`).
- proxy · router: the loopback daemon (`gateway proxy`) · the agent + model pick (`gateway`).
- wire (`providers.WireFor`): Claude → `anthropic`, Responses-only → `responses`, else `openai`. The Databricks wire
  is `serving`; `ai-gateway` is refused. `gateway` names the verb namespace only.
- default · agent record: what the harness picks when nothing is set · the user's choice, one vault record per agent
  (profile, model, effort, context, account, aliases; `vault ls` AGENT table, `configure` agent pages). No profile =
  off; claude is always on. Records are read from the vault, never inherited env; no last-used profile exists.
- Label: `<agent> → <provider>·<profile>/<model>`, e.g. `copilot → databricks·dev/databricks-claude-opus-5-5`.
- Vault secret name: the tree path flattened, stopping at auth; a profile named after its provider is written once
  (`DATABRICKS_PROD_OAUTH`, `ANTHROPIC_KEY`).
- Vendor-read env (`ANTHROPIC_*`, `CLAUDE_CODE_*`, `COPILOT_*`) is exported only at launch, never renamed; exported
  `CLAUDE_PROFILE` marks the session, it is not a setting.

Retired words: runner, vendor (→ agent); router for the daemon (→ proxy); slot, tier (→ model alias; "tier bucket"
stays: an alias's ordered Databricks list in `internal/assets/router/table.json`); leg (→ dispatch); family (→
provider); pin, preference, saved override (→ agent record; the `models --pin` flag keeps its name); first-party (→
Anthropic, or the agent's own login).

| Provider | Profiles | Auth | Secret |
| --- | --- | --- | --- |
| anthropic | `ultra` · `anthropic` | `cli` · `key` | `CLAUDE_CODE_OAUTH_TOKEN` (optional, headless Max) · `ANTHROPIC_KEY` |
| databricks | `dev`, `qa`, `prod` · service principals | `oauth` · `m2m` | `DATABRICKS_<PROFILE>_OAUTH` · `DATABRICKS_<PROFILE>_M2M` |
| github | `copilot` (own login), one row per GitHub username | `cli` (gh / `copilot login`) | `COPILOT_GITHUB_TOKEN` (optional; beats gh's active account) |
| openai | `chatgpt` · `openai` | `cli` · `key` | none · `OPENAI_KEY` |
| google | `antigravity` | `cli` | none |
| magnitude | `local` (127.0.0.1:10100, chat only) | `key` (`echo local`) | none |

opencode's go/zen accounts stay outside the tree for now (`OPENCODE_API_KEY`).

A Claude subscription keeps Remote Control; API keys pay per token. For headless Max, configure mints
`claude setup-token` into vault `CLAUDE_CODE_OAUTH_TOKEN`.

## Providers

Built-ins: `internal/assets/providers/defaults.json` (use its provider-keyed shape). The vault's provider overlay
(rows from `configure github` and `configure databricks`) replaces a built-in **by name**; `null` removes it. A
profile sets `protocol` (`anthropic|openai|responses|gemini`), `base_url`, extra protocol `routes`, `auth` (may set
its own header), model aliases and upstream `headers`. The registry sets `default` and a `fallback` ladder (which
uses the proxy).

```bash
claude-code doctor                               # binaries, providers, credentials and proxy
claude-code vault ls providers [name]            # provider rows and credential state
claude-code configure provider --all [<profile>] # sign-in checks; --check requires all to pass
claude-code switch <profile> [--dry-run]         # session profile; --profile selects one launch
claude-code models --pin opus=<id>               # alias pin; --pin all=<id>; --dry-run previews; --unpin clears
claude-code models --live --profile <p>          # upstream listing
claude-code models discover [--profile <p>]      # live preview; --apply saves additions
claude-code models learn                         # router/fit.json; 14-day shadow, then auto reorders
```

Claude alias pins steer each profile's slot; an unpinned Databricks slot uses Claude Code's id, mapped by the proxy.

**Discovery.** `models discover` lists authenticated live catalogs with bounded pagination and timeouts and
per-profile errors/skips; no fallback catalog is saved. Preview is the CLI default; `/discover-models [--profile p]
[--dry-run]` applies by default. `--apply` appends choices to sealed, versioned records scoped by profile, routes,
spelling and account/auth configuration; an unchanged apply adds nothing; a credential-only change keeps history, a
route/account change selects another scope. It preserves aliases, selections, pins, routes and the legacy global
menu. Saved choices feed configure and agent pickers and routed OpenCode catalogs; Codex includes Responses entries
with bundled metadata only; native vendor catalogs and OpenCode hub launches stay unchanged; unsupported CLI-only
adapters are skipped. Copilot needs an explicit profile login and that account's `gh` token, never changes the
global account pin, and saves additions as history only (its picker uses the live enabled catalog). Gemini-native
profiles list generation models, not embeddings. Missing entries stay saved.

**A listing proves neither inference access, tool support nor current availability.** Verify a route with a real
`claude-code agents ask`, never `curl`.

**Dropped ids.** `--drop <id>[,<id>]` removes saved-menu ids and blocks re-adding; `--undrop` reverses it (`--apply`
writes). On Databricks `--apply` probes new ids and `--verify` also probes saved ones (4 workers, 30 s each); preview
never probes. Only an explicit 400 `This endpoint <id> is deprecated` or 403 `... available only via Unity Gateway`
drops an id; the proxy records the same drop, reason and date from a real response. Pickers and auto skip dropped
ids, discovery shows the reason, `opencode catalog sync --apply` skips them and `catalog add` refuses them until
`--undrop`.

**Probe verdicts.** `models discover --apply|--verify`, `databricks check` and `opencode catalog sync --verify`
stream when supported. A 2xx needs content and a wire terminator: Anthropic `message_stop`, Responses
`response.completed|response.incomplete`, chat `[DONE]` or `finish_reason`; non-chat endpoints keep one
non-streaming request. No content, no terminator or an error frame is `broken`, never auto-dropped; the others are
`ok`, `deprecated`, `fail`, `transient`, `missing`. `databricks check` sends one Anthropic `max_tokens=1` message
and never waits out a 429.

### Vault

`state/vault.db` holds custom launch env, provider/agent overlays and `key_command` as one JSON document, plus
sealed secrets. `configure` edits env, records and rows; an env value may name `@secret` for the vault secret of
that name. Agent records carry route objects (`provider`, `profile`), model, effort and alias pins. Verbs: `ls`,
`get`, `rm`, `import`, `export`, `rotate-secret`.

## Credentials

First non-empty source wins, name by name: the canonical secret name (`ANTHROPIC_KEY`) as process env, then in the
vault (`claude-code vault set secrets <KEY>`, AES-256-GCM); then `auth.env` (`ANTHROPIC_API_KEY`) the same way; then
`auth.file`; then `auth.command` (prints the credential; cached `auth.ttl` seconds, default 300, re-run on 401).
Sealed `oauth`/`m2m`/`pat` credentials come from the vault only; `cli` profiles use the CLI's own login.

Example: `"auth": {"type": "key", "command": "op read op://Private/example/credential", "ttl": 3600}`.
`claude-code resolve --secret --profile <p>` is Claude Code's `apiKeyHelper`; no key lands in `settings.json`.
`vault ls providers` and doctor name the source, never the value. Credential verbs (`vault get`, `resolve`, `gateway
proxy --client-token`, `tailscale --token`) redact unless `--secret`, which only the main session may use. Leaves
receive credentials at launch through vault env `@secret` or overlay `set` with `@secret:<NAME>`.

```bash
claude-code vault set [secrets KEY]                  # bare on a terminal = guided dialog
claude-code vault get KEY... [--secret [--export]]   # --secret: main session only
claude-code vault import keys.env                    # then delete the plaintext file
claude-code vault get --secret KEY... | ssh box claude-code vault import - --yes
claude-code doctor --scan|--git|--secrets            # leak audits; never print values
```

- **Databricks:** `oauth` seals refresh and access tokens together; `pat` is sealed; `m2m` mints from its sealed
  service-principal secret. `claude-code databricks token [--profile <id>] [--auth oauth|m2m|pat] [--copy]
  [--clear-after 30s]` shows type, length and expiry only; a copy clears after 30 s and avoids WSL Win+V history
  (a clipboard manager or a killed clear process can retain it). `configure` → databricks offers Set up (sign-in,
  verify, apply), Login, Paste PAT, Token, Copy token, Apply to agents and Identity.
- **The vault key:** 32 random bytes from `HARNESS_VAULT_KEY` → OS keyring (service `claude-code`) → `key_command`
  (a JSON argv, no shell). No key file is written; a headless box sets the first or the last. `HARNESS_VAULT_KEY` is
  moved out of every `claude-code` process's env at start (`vault.AdoptEnvKey`): only the launched Claude Code
  session gets it. `vault rotate-secret` re-seals and keeps old keys.
- **Transcript cleanup:** `claude-code doctor --secrets --fix` masks idle Claude and harness transcript stores in
  place at the same byte length; optional paths restrict the pass (Codex and Copilot stores are not enabled yet).
  `--secrets` scans transcripts modified within seven days (`--days N`; `--days 0` = all ages) and prints each
  store's counts and older-file skips; `--since N` limits git commits only.

### Environment overrides

Read directly from process env (not pins; set them in the shell or the vault's `env`):

- `HARNESS_KEEP_ENV`: comma-separated names an agent CLI child keeps; codex, copilot, gemini and opencode otherwise
  drop every inherited `*_KEY`, `*_TOKEN`, `*_SECRET`, `*_PASSWORD` and `*_CREDENTIALS` name except their own login
  (launch values are added after the scrub).
- `COWORKER_OVERLAY`: dispatch document (`agent.md` → Overlay).
- `AGENT_ERROR_FALLBACK`: process env beats vault env; values under Routing precedence → Fallback.
- `HARNESS_KEYRING=off`: skip the OS keyring when finding the vault key.
- `HARNESS_HEADERS`: one `Name: value` per line, ≤ 4 KiB, added to upstream requests; invalid lines are dropped and
  auth, attribution and framing headers cannot be overridden.
- `HARNESS_EGRESS_HOST`: set by `--remote <h>`; Databricks workspace requests leave through that ssh host.
- `HARNESS_REMOTE_BIN`: the remote `claude-code` binary for `--host` runs.
- `CLAUDE_REMOTE_HOST`: comma-separated; the first entry is what `--host auto` and `--remote auto` name
  (`rules/harness/network.md`).
- `HARNESS_MODEL_CACHE_TTL`: Go duration a saved discovery snapshot stays fresh for launches and copilot (`24h`).
- `HARNESS_USAGE_CACHE_TTL`: seconds a usage fetch stays fresh (60; 0 = always refetch).
- `HARNESS_USAGE_WARN_PCT`: quota warning threshold in percent, `(0,100]` (80).
- `HARNESS_ALLOW_OPENCODE_2X`: any value lets one install link an opencode newer than 1.x.
- `CLAUDE_RECALL_SEMANTIC=0`: memory recall ranks by keyword only.
- `OPENCODE_DB`: opencode's database: a file name under its data dir, an absolute path, or `:memory:`.
- `DATABRICKS_CONFIG_FILE`: Databricks CLI config file (`~/.databrickscfg`).
- `CLAUDE_WL_PASTE_PROXY`: `wl-paste` proxy mode: `0` off, `1` intercept, `reject`; unset = automatic.

## Routing precedence

- **One interface:** flags (`--profile`, `--model`, `--as`, `--fallback`, `--host`, `--remote`), `COWORKER_OVERLAY`,
  waves, coworker specs, `configure` menus and agent records all normalise into one overlay whose route is one unit,
  taken whole from one source; launch, preflight, labels and `ran on` read it. Inspect the first hop and model:
  `claude-code gateway resolve <agent> [--model m] [--provider P] [--profile p] [--overlay '{...}'] [--all-profiles
  [--live]] --format json`.
- **Model string (the one rule):** only a plain model id, an alias or `auto`; the profile/provider travels as the
  route (`--overlay '{"provider":"databricks","profile":"qa"}' --model opus`, `--profile`, `--provider` or the agent
  record). A prefixed value such as `dev:opus` is refused at every input, naming that overlay form. `auto` rotates
  models only, never the route. Every Claude alias resolves on the session's own profile: a cross-profile alias pin
  is ignored with a note.
- **Model order:** `--model` > an external agent's shell `<AGENT>_MODEL` > the record's `model`. `switch` and
  `<agent> profile` write the record.
- **Profile order** (the route comes whole from the first source naming any of it): `--profile` or overlay `profile`
  (provider from that row, never the session) > overlay `provider`, then `account` > the session profile (with its
  model for the agent) > the agent record's profile or account > the registry default > the agent's own sign-in row.
  Overlay `{"provider":"databricks","profile":"auto"}` or a pool pin picks the pool before these (first valid
  workspace in vault order dev, prod, qa; least-busy under `auto`); a record on the agent's own sign-in row
  (copilot's GitHub row, gemini's Google row) skips the session.
- **Typed model checks:** absent from a fresh listing = refused under any fallback, nearest ids named; no fresh
  listing = unverified; an unknown id on an own-login or custom row with no listing or snapshot is noted, not
  refused. A runtime-gone model is refused under any fallback; a GitHub login's typed model is checked against its
  Copilot catalog snapshot.
- **Spelling:** Databricks rows `databricks-<canonical>` (a bare id gains the prefix only when the workspace serves
  it); first-party Anthropic bare; GitHub, ChatGPT and agy dotted. Claude 5+ is 1M by default; `[1m]` only for
  opus/sonnet 4.x.
- **Fallback** (`AGENT_ERROR_FALLBACK`, `--fallback`, overlay `fallback`): `none` | `pin` | `agents` | `all`
  (`1`/`true`/`yes` = `all`). Unset = `agents` for a bare top-level ask (`router.BareFallback`); a dispatched child
  or an overlay-confined ask keeps `none`. `pin` = requested route > agent record > record `fallback` > stop. Set it
  in `claude-code configure` or `configure agents <agent> profile=<p> model=<m> fallback=<p>` (empty clears).
- **Pool table:** `claude-code gateway resolve all --model auto --all-profiles [--task <brief>] [--format json]`
  peeks every rotation row × agent × tier (pool, next, held, last pick) without moving rotation history; claude has
  no rows (its `auto` runs the brief's tier model on its one profile).
- **Labels:** `ran on` names what served, each fallback hop included; dispatch labels and the session-start per-type
  labels come from the same resolver.

## Model selection

Name the alias (`opus`/`sonnet`/`haiku`/`fable`), never a concrete id: `defaults.json` plus the vault overlay map
each alias per provider.

- On a Databricks profile an alias picks the first member of its bucket (`internal/assets/router/table.json` →
  `databricks`) the agent can run and, with a fresh listing, the workspace lists READY. An explicit `--model`,
  overlay `model` and record model outrank the bucket.
- A GPT family name (`sol`, `astra`, `terra`, `luna`) is the highest numeric version the row's listing serves READY
  (`6.1` = `6-1` < `6.2` < `7`); no listing or member falls back to today's id. A concrete id stays that id.
- Use `opus` for architecture and max-reasoning research; `sonnet` for planning, review, orchestration and
  exploration; `haiku` for frequent cheap leaves; `gemini` for sweeps beyond a session window (1M tokens).

## Served-aware Databricks routing

`configure routing` → `servedAware` (off by default) checks codex, copilot and opencode picks against the
workspace's cached endpoint listing (`state/harness/preflight/served-<profile>.json`, fixed 24h TTL; a GitHub
login's auto pool ages by its Copilot snapshot, `HARNESS_MODEL_CACHE_TTL`). Off, cold or stale reads as the static
menu for ordinary picks; routing never touches the network. Auto pools and cold, stale or empty listings:
`agent.md` → Model precedence.

- **Preflight:** credential-bound successes are reused from router state for 15 minutes; failures and unknowns
  expire after 30 seconds and never block a launch. A token must cover the run timeout plus 2 minutes after
  keepalive or a service-principal mint, unless one of those just renewed it (then a note, not a refusal).
  Preflight refuses an unlisted name; a cold listing skips the check with a note.
- **Listing facts:** the cache keeps each endpoint's `api_types`, `model_class` and the context and output limits
  parsed from its description. A fresh listing's `api_types` decide "serves Responses" for codex and
  `ResponsesServable` (Grok stays out of codex; opencode's routing is unchanged); a catalog limit beats the
  listing's; no listing, old rule.
- **Refresh:** `make install`, `doctor --refresh`, `models preflight --live`; switched on, also `switch`, `<agent>
  profile`, `configure` exit and a detached `models preflight --refresh` at session start (≤ once per 15 minutes).
  That detached child also runs the daily learn steps (`models learn` fold, recall weights), stamped
  `state/harness/preflight/learn-daily`.
- **A swap ignores the alias:** an alias-derived pick (no `--model`, overlay `model` or record model) that is not
  READY is replaced by the first READY entry in menu order, so a sonnet dispatch can land on an opus endpoint; menu
  order is the control. A pinned or explicit model that is not READY is never swapped: its candidate is skipped
  (`<m> is not served on <p>` in `gateway` skips).
- **Live rescue:** a codex `ENDPOINT_NOT_FOUND` marks the model unserved for 1h and retries once on the next READY
  Responses model; an exact `--model` never retries.
- An OpenCode own-plan usage limit fails in seconds as `quota`, holding `opencode·go` until its reported reset.

## Databricks routes (key rule)

- Every agent uses the workspace serving endpoints, never a gateway product (an `options.wire` other than `serving`
  is refused): claude `<workspace>/serving-endpoints/anthropic`, codex `/responses`; copilot and opencode pick per
  model (Claude `/anthropic`, Responses-only GPTs `/responses`, the rest `/chat/completions`).
- gemini (agy) speaks chat/completions through the proxy's `openai` passthrough: on a Databricks profile it runs any
  chat model the workspace serves when one is typed with `--model` (or `--model auto`); with no model or a tier
  alias it runs its Gemini pin. The Responses-only models (`gpt-6-1-sol`, `gpt-6-astra`, `gpt-6-sol`, `gpt-5-5`,
  `gpt-5-5-pro`) are refused for it; run them with codex, copilot or opencode.
- With no `--profile`, gemini sends a typed `databricks-*` model to the session's Databricks gateway row and a
  Gemini model to the Google sign-in row (unless a `GEMINI_PROFILE` gateway pin holds); an unprefixed `claude-*` or
  `gpt-oss-120b` follows the profile in force. The profile in force picks the backend, never the model name.
- On its Google sign-in row agy's models are typed as catalog id plus effort (`gemini-4-hydrogen-xhigh`,
  `claude-sonnet-5-5-medium`, `gpt-oss-120b`); accepted efforts live in `internal/assets/models/catalog.json`
  `efforts`, and an unlisted one fails before agy launches. `--autonomous` is dropped (agy's `--mode auto` is
  account-gated). agy signs in by running `agy` (there is no `agy auth login`).
- On the Google route `gateway resolve`, `agents label` and `usage` read `agy -p /usage` (cached 10 min, spends no
  quota): the note carries the pool's percent left, under 15% warns, and a model at 0% is held until its reset.

## Databricks service-principal identity

- A u2m (`oauth`) row may pair a service principal: `auth.spClientId` plus sealed secret `DATABRICKS_<PROFILE>_M2M`.
  `identity` is `auto` (default) | `user` | `sp` (`claude-code databricks identity --profile <p> auto|user|sp`).
  Precedence: `--as` > the row's `identity` > `auto`.
- `auto` serves the paired SP first when no user login is stored or a dead login is recorded; otherwise the user
  token, then `oauth.MintM2M` on rejection, login-required or an unreadable vault, with a one-line stderr note.
  `user` never mints the SP; `sp` mints only the SP. The SP never refreshes the user login.
- Headless dispatch (`agents ask`, `--batch`, `--background`, `--live`, `--host`) with no `--as` runs a paired
  `auto` row as its SP first, falling back to the user login with a note when the secret is missing or the mint is
  rejected.
- No credential path opens a browser except `databricks login` on a row with no stored SP secret (or `--as user`,
  or `--remote`) and an interactive prompt; on an `sp` row, or an `auto` row with a stored SP secret, `login` mints
  the SP. `--remote <host> databricks login` always signs the user in and seals the token here; `--host` forwards
  the callback port. A browser login with none of `--remote`, `--no-browser` or `--paste` takes the first
  `CLAUDE_REMOTE_HOST` entry as `<host>` (not on that host itself). Flags: `claude-code databricks login -h`.
- A dead user login (`needs_login`) is stored with its times in `state/harness/cache/login-state/<profile>.json`;
  doctor, `databricks check --all`, the status line and proxy errors report it. No timer re-probes it. Sealing a
  login, including `vault set`/`import` with `refresh`, clears it.
- **Token states:** a cached token is fresh until expiry minus min(lifetime/2, 20 minutes) (10 minutes when the
  lifetime is unknown). Inside that window the proxy serves the current token at once and runs one background
  refresh per profile; from expiry minus a skew (1 minute, at most half the window) callers wait for an in-line
  refresh. A failed background refresh cools down for 1 minute while the valid token keeps being served. A
  short-lived command refreshes in line at the same early window. A running proxy also refreshes profiles used in
  the last 24 h once under 15 minutes remain, under the per-profile lock.
- **A sent exchange finishes:** a u2m refresh token is single-use, so a sent exchange always runs to its vault
  write. Callers and a proxy stop cannot cancel it; proxy stop, CLI exit and the launcher's exec wait up to 2
  minutes 15 seconds. A failed vault write reports the rotation as unsaved (`seal_failed`); a rotated token is
  carried until sealed, and a stale one never replaces a newer vault copy. A process killed between send and save
  loses that login: renew with `claude-code databricks login --profile <p>`.
- **Throttling:** a service-principal mint (429, 5xx) and endpoint listing (429, 502, 503, 504) try 3 times, honour
  `Retry-After` (waits capped at 8 s), else back off from 500 ms with full jitter, capped at 4 s. A u2m refresh or
  login code is resent only when the request was never written, never after any HTTP reply. Model traffic uses the
  proxy's cool-down instead (`Retry-After`, 30 s default, 5 minutes at most).
- **One re-mint per rejection:** a 401/403 refresh adopts a token a peer minted after the rejected one instead of
  minting again (in-process under the flight lock; across processes via the sealed vault copy).

## Proxy

`claude` launches **direct** when the provider speaks `anthropic`, every model alias stays on it and no `fallback`
is set. Otherwise it goes through the loopback proxy (`127.0.0.1:18765`, started on demand), which:

1. requires a loopback `Host` and a scoped token (Session tokens below);
2. resolves a plain model id or alias on its route and strips `[1m]`;
3. passes `anthropic` routes through; translates Messages ⇄ chat-completions for `openai` routes (streaming, tools);
4. refreshes the credential and replays once on 401/403 (the error names the remedy for the row's auth type);
5. fails over along `fallback` on 429/5xx/connect error before the first byte;
6. on a hard-quota 429 demotes opus → sonnet → haiku and skips the dead model for 30 minutes;
7. returns an upstream 400 `prompt is too long` as a clean `invalid_request_error`, never retried.

Provider hostnames resolving into the local network are refused (a literal IP or `localhost` is allowed). The proxy
disables Remote Control, so a subscription session stays direct under a `fallback` ladder. Upstream requests carry
`User-Agent: claude-code/<version> <agent>`.

```bash
claude-code gateway proxy [--start|--stop|--restart|--status]   # bare = --status
claude-code gateway proxy --serve --port 18765        # foreground
claude-code gateway proxy --serve --scope <team>@<id> # standing scoped proxy on a free port, until signalled
claude-code gateway [--model M] [--task "<text>"]     # agent + model pick
```

The shared daemon and standing scopes spawn from installed `bin/claude-code`, never a lane or `/tmp` build. `make
install` runs `gateway proxy --restart --if-changed`; the status line and doctor compare `proxy.json`'s executable
SHA-256 with the installed binary, and the first idle poll replaces a stale or down daemon. Streams drain for up
to 2 minutes while the replacement serves; `--stop` waits as long. The replacement loads new token code.

`claude-code gateway jev` operates the Open-Jev classifier (flags: `-h`; bare = `--status`). `--train` starts detached
and watches (Ctrl-C only detaches). `--serve CHECKPOINT` takes only a run's base or a gold-gate pass, installs a
macOS login item named by `--service FILE` and restores the old item if the server stays down. `--trainer DIR --out
DIR` are remembered after first use. `train-jev.sh` is a thin front for it; `doctor` has a `jev` row.

### Session tokens

- Signed, not stored: HMAC over profiles, pid and its start time, issue and expiry (one year; the process is the
  real limit) under a key derived from the router secret and the config dir, so every daemon of the config accepts
  them. A token cannot be revoked: it ends when its process exits, at expiry, or when the router secret rotates (a
  leftover `revoked.json` is ignored). Rotating `CLAUDE_CODE_ROUTER_SECRET` refuses every live signed token once (a
  passthrough session then needs a relaunch).
- Every 401 the proxy itself gives carries `X-Claude-Code-Proxy-Refusal: session-refused`. `resolve --proxy`
  re-mints a refused armed token with its own scope (only its route's profile when the claims cannot be verified);
  a headless run re-mints once; neither marks the provider.
- A token the previous (stored-token) build minted keeps its old rules (7-day idle expiry, bound process, never
  re-minted by `resolve --proxy`, unaffected by the secret) while `sessions.json` lists it. That file is read,
  never written; renewed expiries persist in `legacy-renewals.json`, which only this build writes.
- A token carries the launch's confinement fence (`fz`: the profiles the overlay confines it to; `open` when none).
  A scoped proxy verifies and signs with the same key, binds a token it signs to its own pid and start time, and
  never widens a fence.

## Proxy teams and scopes

Schema: `internal/proxy/proxyteam`; shipped rows: `internal/assets/proxy/teams.json`. The default `shared` team
takes unclaimed kinds; `phase` takes `group`, runs in its owning wave/batch and falls back to `shared`.

- `name`: unique; a scope is `<team>@<id>` (ids: lower-case letters, digits, `.`, `_`, `-`).
- `kinds`: `interactive`, `single`, `lane`, `nested`, `carried`, `group`, `memory`; a dispatch takes the first true
  of group, memory, carried, nested, lane, else `single`.
- `host` / `lifetime` (paired one to one): `daemon`/`shared` (`proxy.json`), `phase`/`phase` (in-process in its
  owner), `detached`/`standing` (standing scoped process), `ask`/`ask` (in one ask; unused).
- `reserved`: proxies kept for the team; a detached slot is `fnv32(dispatch id) % reserved`.
- `borrow`: `never` (no other team's work goes there), `idle` (overflow only at in-flight 0), `any`.
- `fallback`: team names in order; a dispatch's route list is its team, its fallback, then every shared team, once.
- `default`: exactly one team; it takes every kind no team claims.

Vault overlay `proxy-teams` replaces or adds rows by name. Validation rejects unknown values, duplicate names or
kind claims, anything but one default and one shared team, mismatched host/lifetime, and unknown, self or cyclic
fallbacks; `gateway proxy --status` reports invalid config and fails install. Runtime keeps the last clean config
(`state/harness/proxy/teams.last-good.json`, saved on each clean load), else the embedded one; doctor names it. A
scope record sits in `state/harness/proxy/scopes/<team>@<id>.json` while its proxy serves. Standing proxies:
`network.md`.

## Sync the Databricks model menu

```bash
claude-code opencode catalog sync --profile dev [--dry-run|--apply] [--refresh] [--verify]
claude-code opencode catalog check --profile dev --all
```

- **Scope:** Databricks Serving Endpoints and existing authentication only. `--profile` is required (no pin
  fallback, no positionals); other profile types are rejected before any request. Launches consume the menu without
  sync; Copilot's registry separately validates identity each launch and lists on a missing or stale 24h snapshot
  or a model miss. Gateway listings do not establish Serving Endpoints availability.
- **Modes:** the default previews from the authenticated cache or discovers on a miss; `--refresh` forces discovery
  with no stale fallback; `--verify` probes eligible entries; `--apply` saves the snapshot and results and adds
  non-failures. `--apply` and `--dry-run` are exclusive. Preview saves no discovery, probes or menu (authentication
  may refresh credentials). Apply seals new, refreshed or verified snapshots even with no additions; unchanged hits
  are not rewritten; a discovery or auth failure saves nothing.
- **Identity:** every sync validates SCIM `Me` (HTTP 200, nonempty identity ID, not explicitly inactive). That
  proves identity, not endpoint permissions; a refreshed token for the same identity can reuse discovery.
- **Cache:** discovery and sanitized probes are encrypted vault `cache` records (`vault ls cache`), apart from the
  plaintext menu. Key `model-discovery/v1/<sha256 JSON scope>` covers profile, CLI profile, normalized workspace,
  spelling rule, resolved Anthropic/chat/Responses URLs and subject ID; a route change invalidates reuse. Snapshots
  hold no token, fingerprint, raw error or secret prefix. No TTL: use `--refresh` for catalog changes. An unlocked
  vault key is required; auth or decryption failures fail closed. Refresh may replace bad JSON or schema only after
  decryption, never inaccessible ciphertext, and keeps readable historical probes until `--verify` replaces them.
- **What is added:** non-gateway `READY` endpoints with task `llm/v1/chat` and `databricks-` names that round-trip
  to the exact serving name, sorted and deduplicated, then missing ones appended. Existing order, labels and
  explicit Responses flags are kept, even for absent endpoints; new entries infer the flag, chat-only ids never use
  Responses, redundant flags are not stored. Nothing is removed. Failed probes, historical cached ones included,
  block additions; a listing-only entry with no known failure may be added as discovered/unverified. An empty or
  no-change run does not write the menu.
- **`--verify`:** uses the existing Responses flag, or infers for new entries: `/responses`, else Claude's native
  `/v1/messages` when available, else `/chat/completions`. Routes are the resolved ones, never hardcoded bases, and
  must stay under the workspace's HTTPS `/serving-endpoints` with no redirects. Four workers, 30 s per request,
  5 min overall; token budgets 256 chat / 128 Responses / 32 Anthropic; an HTTP rejection retries without tools.
  Success proves HTTP and tool-definition acceptance and a clean stream, not a tool call or a correct answer;
  budget or API limits may fail it, with no model-specific exceptions. Only status, HTTP code, tool-definition
  acceptance and latency are saved. After a successful manual verification `catalog add --responses` can select
  Responses. `catalog check` keeps its legacy routes, 8/16-token budgets and result storage.
- **Use:** the menu overlay is shared across profiles. OpenCode uses all entries; Codex uses Responses entries with
  bundled metadata. Relaunch the CLI after applying; confirm access with `check --all`.

## Databricks read-only API and cost

**The `databricks api` verb.** `claude-code databricks api <path> --profile <p> [--as user|sp]` is an authenticated
GET, so no one reaches for `curl` with a token. Databricks profiles only. The path must be plain: no scheme, host,
escapes, `..` or `//`; a query may hold only letters, digits and `_=&.,-`. No redirect is followed, the body is
capped at 8 MiB, the token is never printed, and 429, 502, 503 and 504 retry up to 3 times. Allowed prefixes and
maximum extra path segments:

```text
/api/2.0/serving-endpoints            1
/api/2.0/sql/warehouses               1
/api/2.0/sql/statements               1
/api/2.0/preview/scim/v2/Me           0
/api/2.1/unity-catalog/metastore_summary  0
/api/2.1/unity-catalog/metastores     2   (metastores/<id>/systemschemas)
/api/2.1/unity-catalog/catalogs       1
/api/2.1/unity-catalog/schemas        1
/api/2.1/unity-catalog/tables         1
```

The serving-endpoints listing includes each endpoint's creator address: do not paste its output off the machine.

**Cost keys.** Per Databricks profile (others are refused), USD per million tokens; an empty `warehouse_id` clears
it and `options.prices={}` clears the table. `warehouse_id` is 1–64 letters/digits; rates must be finite and
nonnegative.

```bash
claude-code configure provider dev options.warehouse_id=<id> \
  'options.prices={"<model-id>":{"input":0,"output":0,"cache_read":0,"cache_write":0}}'
claude-code vault ls providers dev          # shows options.warehouse_id and each price
claude-code usage --cost --days 7           # estimate per profile and model
```

`usage --cost` estimates local ledger tokens × profile prices, counting 1-hour cache writes as writes; it is not an
invoice. There is no system-table billing report: never request grants; use paired SPs for Databricks needs.

**Ledger stamps.** Every metered row of `state/usage/ledger.jsonl` carries `first_ms` (send to the first client
byte), `total_ms` (send to the ledger write) and `cost_usd`, all of the attempt that served. `cost_usd` is the
profile's `options.prices` row for the served model, else the Databricks list price, else 0 (another provider's
list price is not used). A row with no token counts is not written; older rows omit the three fields.

**Route weights.** The pick scores price, headroom and latency, default 0.4, 0.4 and 0.2 (`configure routing
weights.price=<0-1>` and `weights.latency=<0-1>`; headroom is the remainder so the three sum to 1). Latency is the
blended first-event time, (p50+p95)/2 of `first_ms`, over the last 7 days and at least 5 rows in the ledger's last
2 MiB: the fastest measured candidate scores 1, a slower one fastest/own. With no measured candidate the latency
weight drops out and price and headroom are rescaled; an unmeasured candidate among measured ones ties at 1. A
weights map saved before latency existed still loads (missing latency is 0).

**Spend cap.** `routing.spendCap.sessionUSD` and `spendCap.dayUSD` cap metered USD per session and per local day;
off until set (`configure routing spendCap.sessionUSD=<usd>` / `spendCap.dayUSD=<usd>`; empty clears one), each in
(0, 1000000]. At a cap the proxy drops metered rows from the attempt ladder and tries any remaining unmetered `cli`
subscription rows; with none left it refuses with 402 `billing_error` and `X-Claude-Code-Proxy-Refusal: spend-cap`.
Each metered failover hop rechecks the cap before calling upstream; a cap reached during failover refuses with 402
and ends the ladder. Subscription rows and `count_tokens` are unaffected; a request with no session id is held only
by the day cap. A ledger read error fails open and logs `proxy: spend cap unchecked`. Doctor reports caps and
today's spend. Costs land after calls, so in-flight requests can overshoot by their cost.

**Gemini built-in tools.** `claude-code configure provider <p> options.google_tools=url,code` records the built-ins
a Databricks row serves, from a probe (`search|url|code`; empty clears it). Unset means unprobed: the proxy sends
what the caller asked (no warning) and the upstream answers. Chat routes strip and ignore the Google tools header.
Search is refused when cross-region processing is disabled; web search stays Tavily
(`internal/proxy/server/websearch.go`).
