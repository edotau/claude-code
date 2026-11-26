---
paths:
  - "internal/providers/**"
  - "internal/vault/**"
  - "internal/proxy/**"
  - "internal/databricks/**"
  - "internal/catalog/**"
---
# Providers Reference

Provider schema, credentials, discovery and proxy behavior live here; route precedence: `providers.md`.

## Providers

Built-ins live in `internal/assets/providers/defaults.json`. The vault's provider overlay (rows added by
`configure github` and `configure databricks`) overlays them **by name** (an entry replaces the built-in; `null` removes
it). Each entry is a **profile** (an account on a provider: `qa`, `ultra`, `chatgpt`); see [Naming](#naming).

Each profile sets `protocol` (`anthropic|openai|responses|gemini`), `base_url`, extra protocol `routes`, `auth`,
model aliases and upstream `headers`; `auth` may set its own header. The registry sets `default` and a `fallback`
ladder (which uses the proxy).
Use the provider-keyed shape in `internal/assets/providers/defaults.json`.

```bash
claude-code doctor                              # binaries, providers, credentials and proxy
claude-code vault ls providers [name]            # provider rows and credential state
claude-code configure provider --all [<profile>] # sign-in checks; --check requires all to pass
claude-code switch <profile> [--dry-run]          # session profile; --profile selects one launch
claude-code models --pin opus=<id>               # alias pin; --pin all=<id>; --dry-run previews
claude-code models --unpin                       # clear pins
claude-code models --live --profile <p>          # upstream listing
claude-code models discover [--profile <p>]      # live preview; --apply saves additions
claude-code models learn                         # router/fit.json; 14-day shadow, then auto reorders
```

`/discover-models [--profile p] [--dry-run]` applies by default. Launch route/model precedence:
`providers.md` → Routing precedence; dispatch models: `agent.md` → Model precedence.
Claude alias pins steer each profile's slot; an unpinned Databricks slot uses Claude Code's id, mapped by the proxy.
Agent settings are vault records; exported `CLAUDE_PROFILE` marks the session, not a setting.

`models discover` lists authenticated live catalogs with bounded pagination/timeouts and per-profile errors/skips;
no fallback catalogs are saved. Preview is the CLI default. `--apply` appends choices to sealed, versioned records
scoped by profile, routes, spelling and account/auth configuration; unchanged applies add nothing. Credential-only
changes retain history; route/account changes select another scope.

`--drop <id>[,<id>]` removes saved-menu ids and blocks re-adding; `--undrop` reverses it (`--apply` writes).
On Databricks, `--apply` probes new ids; `--verify` also probes saved ones (4 workers, 30 s each). Only an explicit
400 `This endpoint <id> is deprecated` or 403 `... available only via Unity Gateway` drops an id; other failures
change nothing. Preview never probes. The proxy records the same drop, reason and date from a real response without
another call. Pickers and auto skip dropped ids; discovery shows the reason, `opencode catalog sync --apply` skips
and `catalog add` refuses them until `--undrop`.

**Probe verdicts.** `models discover --apply|--verify`, `databricks check` and `opencode catalog sync --verify`
stream when supported. A 2xx needs content and a wire terminator: Anthropic `message_stop`, Responses
`response.completed|response.incomplete`, chat `[DONE]` or `finish_reason`. Non-chat endpoints keep one non-streaming
request. A stream with no content, no terminator or an error frame is `broken`, never auto-dropped; other verdicts
are `ok`, `deprecated`, `fail`, `transient`, `missing`. Explicit drop command:
`claude-code models discover --drop <id> --apply`. `databricks check` uses one Anthropic `max_tokens=1` message and
never waits out a 429.

Discovery preserves aliases, selections, pins, routes and the legacy global menu. Saved choices feed configure and
agent pickers and routed OpenCode catalogs; Codex includes Responses entries with bundled metadata only. Native
vendor catalogs and OpenCode hub launches stay unchanged; unsupported CLI-only adapters are skipped. Copilot needs
an explicit profile login and that account's `gh` token, never changes the global account pin, and saves additions
as history only: its native picker uses the live enabled catalog. Gemini-native profiles list generation models,
not embeddings. Missing entries remain saved: discovery proves neither inference access nor current availability.

### Vault

`state/vault.db` holds custom launch env, provider/agent overlays and `key_command` as one JSON document, plus
sealed secrets. `configure` edits env, records and rows. Env values may name `@secret` for a vault secret of the
same name. Agent records carry route objects (`provider`, `profile`), model, effort and alias pins.
Vault verbs: `ls`, `get`, `rm`, `import`, `export`, `rotate-secret`; env/overlay edits: `configure`.
Safe credential inspection: Credentials below.

## Credentials

First non-empty source wins, name by name: the canonical secret name (`ANTHROPIC_KEY`) as **process env**, then in the
**vault** (`claude-code vault set secrets <KEY>`, AES-256-GCM); then `auth.env` (`ANTHROPIC_API_KEY`) the same way;
then **`auth.file`** → **`auth.command`** (prints the credential; cached `auth.ttl` seconds, default 300, re-run on
401). Sealed `oauth`/`m2m`/`pat` credentials come from the vault only; `cli` profiles use the CLI's own login.

Example: `"auth": {"type": "key", "command": "op read op://Private/example/credential", "ttl": 3600}`.
`claude-code resolve --secret --profile <p>` is Claude Code's `apiKeyHelper`; no key lands in `settings.json`.
`vault ls providers` and doctor identify the source, not its value. Credential verbs (`vault get`, `resolve`,
`gateway proxy --client-token`, `tailscale --token`) redact unless `--secret`, which only the main session may use.
Leaves receive credentials at launch through vault env `@secret` or overlay `set` with `@secret:<NAME>`.

```bash
claude-code vault set [secrets KEY]                 # bare on a terminal = guided dialog
claude-code vault get KEY... [--secret [--export]]   # main session only with --secret
claude-code vault import keys.env                  # then delete the plaintext file
claude-code vault get --secret KEY... | ssh box claude-code vault import - --yes
claude-code doctor --scan|--git|--secrets             # leak audits; never print values
```

Databricks `oauth` seals refresh/access tokens together; `pat` is sealed; `m2m` mints tokens using its sealed
service-principal secret. Vault names: Naming below; identity/refresh: `providers.md` → Databricks service-principal
identity. `claude-code databricks token [--profile <id>] [--auth oauth|m2m|pat] [--copy] [--clear-after 30s]` shows
type, length and expiry only. Copy clears after 30 s and avoids WSL Win+V history; clipboard managers or a killed clear
process can retain it. `configure` → databricks offers Set up (sign-in, verify, apply), Login, Paste PAT, Token,
Copy token, Apply to agents and Identity.

**The vault key** — 32 random bytes from `HARNESS_VAULT_KEY` → OS keyring (service `claude-code`) → `key_command` (a
JSON argv, no shell). No key file is written; headless boxes set one of the first or last; `HARNESS_KEYRING=off`
skips the keyring. `HARNESS_VAULT_KEY` is moved out of every `claude-code` process's env at start
(`vault.AdoptEnvKey`), so no child inherits it; only the launched Claude Code session gets it.
`vault rotate-secret` re-seals and keeps old keys.

### Environment overrides

Process env read directly by the harness (not pins; set them in the shell or the vault's `env`):

- `HARNESS_KEEP_ENV`: comma-separated names an agent CLI child keeps; codex, copilot, gemini and opencode otherwise drop
  every inherited `*_KEY`, `*_TOKEN`, `*_SECRET`, `*_PASSWORD` and `*_CREDENTIALS` name except their own login (launch
  values are added after the scrub)
- `COWORKER_OVERLAY`: Dispatch document; keys, precedence and inheritance: `agent.md` → Overlay.
- `HARNESS_KEYRING=off`: skip the OS keyring when finding the vault key
- `CLAUDE_RECALL_SEMANTIC=0`: memory recall ranks by keyword only (no embeddings)
- `OPENCODE_DB`: opencode's database: a file name under its data dir, an absolute path, or `:memory:`
- `DATABRICKS_CONFIG_FILE`: Databricks CLI config file (default `~/.databrickscfg`)
- `CLAUDE_WL_PASTE_PROXY`: `wl-paste` proxy mode: `0` off, `1` intercept, `reject`; unset = automatic
- `AGENT_ERROR_FALLBACK`: Process env beats vault env; values: `providers.md` → Routing precedence.
- `HARNESS_EGRESS_HOST`: set by `--remote <h>`: Databricks workspace requests leave through that ssh host
- `HARNESS_REMOTE_BIN`: the remote `claude-code` binary for `--host` runs
- `CLAUDE_REMOTE_HOST`: a comma-separated list; the first entry is the machine `--host auto` and `--remote auto` name
  (`rules/harness/network.md`)
- `NETWORK_MEMORY`: off by default: unset or any value other than `on` means no sync or connection; `on`
  (case-insensitive) opts this machine in to memory sync with the hosts in `CLAUDE_REMOTE_HOST`
- `HARNESS_ALLOW_OPENCODE_2X`: any value lets one install link an opencode newer than 1.x
- `HARNESS_MODEL_CACHE_TTL`: Go duration a saved discovery snapshot stays fresh for launches and copilot (default `24h`)
- `HARNESS_USAGE_CACHE_TTL`: seconds a usage fetch stays fresh (default 60; 0 = always refetch)
- `HARNESS_USAGE_WARN_PCT`: quota warning threshold in percent, `(0,100]` (default 80)

## Naming

- agent: the CLI doing the work; `claude` `codex` `copilot` `opencode` `gemini`
- provider: the platform hosting the endpoint; `anthropic` `databricks` `github` `openai` `google` `magnitude`
- profile: an account on a provider; carries billing; unique across all providers, `[A-Za-z0-9_-]`, opaque to code
- auth: how a profile proves who it is; `oauth` (user login) `m2m` (service principal) `pat` `key` `cli` (agent's own
  login) `none`
- model: what `--model` takes; an alias (`opus`, follows the table), a version (`opus-5-5`, pinned), a full wire id or a
  bare served endpoint name (`gpt-6-1-terra`)
- proxy · router: the loopback daemon · the agent + model pick; `gateway proxy` · `gateway`
- wire: the protocol a model rides (`providers.WireFor`): Claude → `anthropic`, Responses-only → `responses`, else
  `openai`; `anthropic` `openai` `responses`
- default · agent record: what the harness picks when nothing is set · the user's own choice, saved in the vault;
  precedence: `providers.md` → Routing precedence; no profile = off, claude always on

Retired words: runner and vendor (→ agent), router for the daemon (→ proxy), slot and tier (→ model alias; "tier
bucket" is the exception: an alias's ordered Databricks list in `internal/assets/router/table.json`), leg (→
dispatch), family (→ provider), pin, preference and saved override (→ agent record; the `models --pin` flag keeps its
name), first-party (→ Anthropic, or the agent's own login). `gateway` names the verb namespace only; the Databricks wire
is `serving` (`ai-gateway` is refused).

- Label (display): `<agent> → <provider>·<profile>/<model>`; `copilot → databricks·dev/databricks-claude-opus-5-5`
- Agent record (vault): one per agent: profile, model, effort, context, account, aliases; `vault ls` AGENT table,
  `configure` agent pages
- Vault secret: the tree path flattened, stopping at auth; a profile named after its provider is written once;
  `DATABRICKS_PROD_OAUTH`, `ANTHROPIC_KEY`

Agent records are read from the vault, never inherited env; vendor-read env (`ANTHROPIC_*`, `CLAUDE_CODE_*`,
`COPILOT_*`) is only exported at launch, never renamed; model ids are built per provider from the canonical
form (`opus-5-5` → `claude-opus-5-5`, `databricks-claude-opus-5-5`, `system.ai.claude-opus-5-5`); code never
parses a profile name.

- anthropic: `ultra` · `anthropic`; `cli` · `key`; `CLAUDE_CODE_OAUTH_TOKEN` (optional, headless Max) · `ANTHROPIC_KEY`
- databricks: `dev`, `qa`, `prod` · service principals; `oauth` · `m2m`; `DATABRICKS_<PROFILE>_OAUTH` ·
  `DATABRICKS_<PROFILE>_M2M`
- github: `copilot` (own login), one row per GitHub username; `cli` (gh / `copilot login`); `COPILOT_GITHUB_TOKEN`
  (optional; beats gh's active account)
- openai: `chatgpt` · `openai`; `cli` · `key`; none · `OPENAI_KEY`
- google: `antigravity`; `cli`; none
- magnitude: `local` (127.0.0.1:10100, chat only); `key` (`echo local`); none

opencode's go/zen accounts stay outside the tree for now (`OPENCODE_API_KEY`).

## Routing

`claude` launches **direct** when the provider speaks `anthropic`, every model alias stays on it, and no `fallback` is
set. Otherwise it goes through the **loopback proxy** (`127.0.0.1:18765`, started on demand), which:

1. requires a loopback `Host` and a scoped token (`providers.md` → Session tokens);
2. resolves a plain model id or alias on its route and strips `[1m]` (precedence: `providers.md` → Routing precedence);
3. passes `anthropic` routes through; translates Messages ⇄ chat-completions for `openai` routes (streaming, tools);
4. refreshes the credential and replays once on 401/403 (the error names the remedy for the row's auth type);
5. fails over along `fallback` on 429/5xx/connect error before the first byte;
6. on a hard-quota 429 demotes opus → sonnet → haiku and skips the dead model for 30 minutes;
7. returns an upstream 400 `prompt is too long` as a clean `invalid_request_error`, never retried.

Provider hostnames resolving into the local network are refused (a literal IP or `localhost` is allowed). The proxy
disables Remote Control, so a subscription session stays direct under a `fallback` ladder. Upstream requests carry
`User-Agent: claude-code/<version> <agent>`; `HARNESS_HEADERS` (one `Name: value` per line, ≤ 4 KiB)
adds upstream headers; invalid lines are dropped, and auth, attribution and framing headers cannot be overridden.

```bash
claude-code gateway proxy [--start|--stop|--restart|--status]   # bare = --status
claude-code gateway proxy --serve --port 18765        # foreground
claude-code gateway proxy --serve --scope <team>@<id> # standing scoped proxy on a free port, until signalled
claude-code gateway [--model M] [--task "<text>"]   # agent + model pick
```

## Proxy teams and scopes

Team schema: `internal/proxyteam`; shipped rows: `internal/assets/proxy/teams.json`. The default `shared` team takes
unclaimed kinds; `phase` takes `group`, runs in its owning wave/batch and falls back to `shared`.

- `name`: unique; a scope is named `<team>@<id>`
- `kinds`: `interactive`, `single`, `lane`, `nested`, `carried`, `group`, `memory`; a dispatch takes the first true of
  group, memory, carried, nested, lane, else `single`
- `host`: `daemon` (`proxy.json`), `phase` (in-process in its owner), `detached` (standing scoped process), `ask` (in
  one ask; unused)
- `lifetime`: `shared`, `phase`, `standing`, `ask`; each host pairs with one lifetime (daemon/shared, phase/phase,
  detached/standing, ask/ask)
- `reserved`: proxies kept for the team; a detached slot is `fnv32(dispatch id) % reserved`
- `borrow`: `never` (no other team's work goes there), `idle` (overflow only at in-flight 0), `any`
- `fallback`: team names in order; a dispatch's route list is its team, its fallback, then every shared team, each once
- `default`: exactly one team; it takes every kind no team claims

**Load.** Vault overlay `proxy-teams` replaces/adds rows by name. Validation rejects unknown values, duplicate names
or kind claims, anything but one default and one shared team, mismatched host/lifetime, and unknown, self or cyclic
fallbacks. `gateway proxy --status` reports invalid config and fails install. Runtime keeps the last clean config
(`state/harness/proxy/teams.last-good.json`, saved on each clean load), else the embedded one; doctor names it.

A scope record sits in `state/harness/proxy/scopes/<team>@<id>.json` while its proxy serves; `--scope` ids are
lower-case letters, digits, `.`, `_`, `-`. Route order: `agent.md` → Proxy routes. Standing proxies: `network.md`.

## Sync the Databricks model menu

Menu sync uses only Databricks Serving Endpoints and existing authentication. It requires `--profile` (no pin
fallback or positionals) and rejects other profile types before requests. Launches consume the menu without sync;
Copilot's registry separately validates identity each launch and lists on a missing/stale 24h snapshot or model miss.

```bash
claude-code opencode catalog sync --profile dev [--dry-run|--apply] [--refresh] [--verify]
claude-code opencode catalog check --profile dev --all
```

Default sync previews from authenticated cache or discovers on miss; `--refresh` forces discovery without stale
fallback, `--verify` probes eligible entries, and `--apply` saves the snapshot/results and adds non-failures.

Every sync validates SCIM `Me`: HTTP 200, nonempty identity ID, not explicitly inactive. This proves identity,
not endpoint permissions; a refreshed token for the same identity can reuse discovery.

Discovery and sanitized probes are encrypted vault `cache` records (`vault ls cache`), separate from the plaintext
menu. Key `model-discovery/v1/<sha256 JSON scope>` covers profile, CLI profile, normalized workspace, spelling rule,
resolved Anthropic/chat/Responses URLs and subject ID. Route changes invalidate reuse. Snapshots contain no token,
fingerprint, raw error or secret prefix. Sync has no TTL: use `--refresh` for catalog changes. An unlocked vault key
is required; auth/decryption failures fail closed. Refresh may replace bad JSON/schema only after decryption,
never inaccessible ciphertext, and preserves readable historical probes until `--verify` replaces them.

Preview/`--dry-run` saves no discovery, probes or menu; authentication may refresh credentials. `--apply` and
`--dry-run` are exclusive. Apply seals new/refreshed/verified snapshots even with no additions; unchanged hits
are not rewritten. Discovery/auth failures save nothing.

Sync sorts/deduplicates non-gateway `READY` endpoints with task `llm/v1/chat` and `databricks-` names that round-trip
to the exact serving name, then appends missing entries. It preserves existing order, labels and explicit Responses
flags, even for absent endpoints; new entries infer the flag, chat-only ids never use Responses, and redundant flags
are not stored. Nothing is removed. Failed probes, including historical cached failures, block additions;
listing-only entries without a known failure may be added as discovered/unverified. Empty/no-change runs do not
write the menu. Dropped-id handling: Providers above.

`--verify` uses existing Responses flags or inference for new entries: `/responses`, else Claude's native
`/v1/messages` when available, else `/chat/completions`. Use resolved routes, never hardcoded bases; they must stay
under the workspace's HTTPS `/serving-endpoints`, without redirects. Four workers use 30 s request and 5 min overall
limits, with token budgets 256 chat / 128 Responses / 32 Anthropic. HTTP rejection retries without tools.
Success proves HTTP/tool-definition acceptance and a clean stream, not a tool call or generated-answer correctness.
Budget/API limits may cause failure; no model-specific exceptions. After successful manual verification,
`catalog add --responses` can select Responses. Save only status, HTTP code, tool-definition acceptance and latency,
never raw errors. `catalog check` keeps its legacy routes, 8/16-token budgets and result storage.

The menu overlay is shared across profiles. OpenCode uses all entries; Codex uses Responses entries with bundled
metadata. Relaunch the relevant CLI after applying. A listing proves neither inference access nor tool support;
use `check --all`. Gateway listings do not establish Serving Endpoints availability.

## Databricks read-only API and cost

**The `databricks api` verb.** `claude-code databricks api <path> --profile <dev|qa|prod> [--as user|sp]` is an
authenticated GET, so no one reaches for `curl` with a token. Databricks profiles only. The path must be plain: no
scheme, host, escapes, `..` or `//`; a query may hold only letters, digits and `_=&.,-`. No redirect is followed, the
body is capped at 8 MiB and the token is never printed. Retry 429, 502, 503 and 504 up to 3 times.
Allowed metadata prefixes and maximum extra path segments:

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

The serving-endpoints listing includes each endpoint's creator address: do not paste its output outside the machine.

**Cost keys.** Per Databricks profile (others are refused), USD per million tokens; an empty `warehouse_id` clears it,
and `options.prices={}` clears the table:

```bash
claude-code configure provider dev options.warehouse_id=<id> \
  'options.prices={"<model-id>":{"input":0,"output":0,"cache_read":0,"cache_write":0}}'
claude-code vault ls providers dev   # shows options.warehouse_id and each price
claude-code usage --cost --days 7           # estimate per profile and model
```

`warehouse_id` is 1–64 letters/digits; rates must be finite and nonnegative. `usage --cost` estimates local ledger
tokens × profile prices, counting 1-hour cache writes as writes; it is not an invoice. No system-table billing report:
never request grants; use paired SPs for Databricks needs.

**Ledger stamps.** Every metered row of `state/usage/ledger.jsonl` also carries `first_ms` (send to the first client
byte), `total_ms` (send to the ledger write) and `cost_usd`, all of the attempt that served. `cost_usd` is the profile's
`options.prices` row for the served model, else the Databricks list price, else 0 (another provider's list price is not
used). A row with no token counts is not written. Older rows omit the three fields.

**Route weights.** The pick scores price, headroom and latency, default 0.4, 0.4 and 0.2 (set with
`configure routing weights.price=<0-1>` and `weights.latency=<0-1>`; headroom is what remains so the three sum to 1).
Latency is the blended first-event time, (p50+p95)/2 of `first_ms`, over the last 7 days and at least 5 rows in the
ledger's last 2 MiB: the fastest measured candidate scores 1 and a slower one scores fastest/own. With no measured
candidate the latency weight drops out and price and headroom are rescaled, so the order is the two-weight one. An
unmeasured candidate among measured ones ties at 1. A weights map saved before latency existed still loads (missing
latency is 0). Commands: `rules/harness/commands.md` → Setup.

**Spend cap.** `routing.spendCap.sessionUSD` and `spendCap.dayUSD` cap metered USD per session and per local day. Both
are off until set (`configure routing spendCap.sessionUSD=<usd>` and `spendCap.dayUSD=<usd>`; empty clears one).
A set value must be in (0, 1000000]. When the session spend (or the day's spend) reaches its cap, the proxy drops
metered rows from the attempt ladder and attempts any remaining unmetered `cli` subscription rows. It refuses with 402
`billing_error` and `X-Claude-Code-Proxy-Refusal: spend-cap` if no unmetered row remains at this check. Each metered
failover hop also rechecks the cap before calling upstream; a cap newly reached during failover refuses the request
with 402 and terminates the attempt ladder. Subscription rows and `count_tokens` are unaffected; a request with no
session id is held only by the day cap. A ledger read error fails open and logs `proxy: spend cap unchecked`.
Doctor reports caps and today's spend. Costs land after calls, so in-flight requests can overshoot by their cost.
Commands: `rules/harness/commands.md` → Setup.

**Gemini built-in tools.** `claude-code configure provider <p> options.google_tools=url,code` records the built-ins a
Databricks row serves, from a probe (`search|url|code`; empty clears it). Unset means unprobed: the proxy sends
what the caller asked (no warning) and the upstream answers. Chat routes strip and ignore the Google tools
header. Search is refused when cross-region processing is disabled; web search stays Tavily
(`/Users/edotau/.claude/internal/proxy/server/websearch.go`).
