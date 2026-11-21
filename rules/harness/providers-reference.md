---
paths:
  - "internal/providers/**"
  - "internal/vault/**"
  - "internal/proxy/**"
  - "internal/databricks/**"
  - "internal/catalog/**"
---
# Providers Reference

Moved from the README: provider file shape, credential order, naming, routing and the Databricks menu sync. The
short commands and precedence are in `providers.md`.

## Providers

Built-ins live in `internal/providers/defaults.json`. The vault's provider overlay (rows added by `configure github`
and `configure databricks`) overlays them **by name** (an entry replaces the built-in; `null` removes it). Each
entry is a **profile** (an account on a provider: `qa`, `ultra`, `chatgpt`); see [Naming](#naming).

```jsonc
"providers": {
  "default": "example",
  "fallback": ["example", "anthropic"],         // 429/5xx ladder (implies the proxy)
  "providers": {
    "example": {
      "protocol": "anthropic",                       // anthropic · openai · responses · gemini
      "base_url": "https://llm.example.com",
      "routes": {"openai": "/v1"},               // extra protocols on the same endpoint
      "auth": {"type": "key", "env": "EXAMPLE_API_KEY"},   // oauth · m2m · pat · key · cli · none; optional header
      "models": {"opus": "claude-opus-5", "sonnet": "claude-sonnet-5", "haiku": "gpt-5-nano"},
      "headers": {}                                  // sent on every upstream request
    },
    "ollama": null
  }
}
```

```bash
claude-code configure provider --list [name]        # providers, credential state, fallback ladder
claude-code configure provider --all [<profile>]    # check every sign-in (or one); --check exits 1 if any not ok
claude-code switch example                          # pin the session profile
claude-code models --pin haiku=gpt-5-nano            # pin a model alias: a plain id on the session profile
claude-code models --unpin | --live --profile p     # clear pins | list what upstream serves
claude-code models discover                         # live preview across all registry profiles
claude-code models learn                            # fold dispatch records into router/fit.json (shadow 14 days, then --model auto reorders by it)
claude-code models discover --profile p --apply     # append profile-scoped runtime choices
# /discover-models [--profile p] [--dry-run]         # slash launcher applies by default
claude-code --profile example --model sonnet         # this launch only
```

Precedence: `--profile` → the claude agent record's `profile` → registry `default`. Model: `--model` → the record's
`aliases` (`models --pin`) → the profile's `models`. Settings live in one vault record per agent; an exported
`CLAUDE_PROFILE` marks the session, it is not a setting.

`models discover` uses authenticated live listing only, with bounded pagination/timeouts and
per-profile errors/skips; it never saves fallback catalogs. CLI preview is the default;
`--apply` saves append-only choices in sealed, versioned vault records scoped by profile,
routes, spelling and account/auth configuration. A second unchanged apply adds nothing.
`--drop <id>[,<id>]` (`--apply` writes) removes saved-menu ids and blocks their re-adding; `--undrop` reverses it.
`--apply` on a Databricks profile sends one tiny request per id it is about to add (`--verify`: also the saved ones;
4 workers, 30 s each) and drops an id answered HTTP 400 `This endpoint <id> is deprecated` (`dropped as deprecated:`);
any other failure changes nothing, and a preview sends no probe. The proxy drops the same way on
its own: the first real Databricks reply `400 This endpoint <id> is deprecated` records the id (reason, date) in that
profile's drop list; pickers and the `--model auto` pool skip it and `models discover` lists it with the reason.
`opencode catalog sync --apply` skips a dropped id (`skipped as dropped:`) and
`catalog add` refuses it; `--undrop` lifts both.

**Probe verdicts.** `models discover --apply|--verify`, `databricks check` and `opencode catalog sync --verify` send a
streamed request (a non-chat endpoint cannot stream and keeps one non-streaming request). A 2xx counts only when the
stream carries content and ends with its wire's terminator: `message_stop` (Anthropic), `response.completed` or
`response.incomplete` (Responses), `[DONE]` or a `finish_reason` (chat). Verdicts: `ok`; `deprecated` (the explicit
400 above); `broken` (accepted, but the stream ended without its terminator, had no content, or carried an error
frame); and the existing error classes (`fail`, `transient`, `missing`). Only the explicit deprecated answer drops a
model under `--apply`. A broken model is reported as `stream check failed, not dropped: <id>: <reason>` with the
command to drop it by hand, `claude-code models discover --drop <id> --apply`. `databricks check` sends one
`max_tokens=1` message on the profile's Anthropic route and prints `ok`, `stream broken: <reason>` or the HTTP class
(a deprecated model shows as its 400 text); it never waits out a 429.
Configured aliases, selections, pins, routes and the legacy global Databricks menu are
not rewritten. Except for Copilot, saved choices feed configure/agent model pickers and harness-generated
OpenCode catalogs on the next routed launch; Databricks Codex catalogs include only
Responses entries for which Codex supplies bundled metadata. Native vendor catalogs
and OpenCode hub-native launches are not rewritten. Vendor CLI-only catalogs without
a supported adapter are explicit skips. Copilot requires an explicit profile login and
uses that account's `gh` token without changing the global account pin. Copilot additions
are saved history only: its native picker continues to use the live enabled catalog. Gemini-native
API profiles list generation-capable models, not embeddings. Saved choices remain even
when absent from later listings: **discovery is not inference proof or current availability**.
Changing route/account configuration selects a separate additions scope; changing only
a credential in the same configuration retains history.

### Vault

`state/vault.db` holds custom launch env, provider/agent overlays and `key_command` as one JSON document, plus
sealed secrets. `configure` edits the env (settings → environment), agent records and provider rows. `vault ls`
shows secrets as `<prefix>***  N chars` and marks failing entries ✗ first (`--format json` keeps empty sections).

```jsonc
{
  "agents": {"records": {"claude": {"profile": "ultra", "aliases": {"haiku": "gpt-5-nano"}},
                         "codex": {"profile": "chatgpt", "model": "databricks-gpt-6-astra", "effort": "medium"}}},
  "env": {"MY_PROJECT_ROOT": "/home/me/work", "SOME_API_TOKEN": "@secret"},   // @secret = vault secret of that name
  "providers": {"default": "example"}, "vault": {"key_command": ""}
}
```

## Credentials

First non-empty source wins, name by name: the canonical secret name (`ANTHROPIC_KEY`) as **process env**, then in the
**vault** (`claude-code vault set <KEY>`, AES-256-GCM); then `auth.env` (`ANTHROPIC_API_KEY`) the same way; then
**`auth.file`** → **`auth.command`** (prints the credential; cached `auth.ttl` seconds, default 300, re-run on 401).
Sealed `oauth`/`m2m`/`pat` credentials come from the vault only; `cli` profiles use the CLI's own login.

Example: `"auth": {"type": "key", "command": "op read op://Private/example/credential", "ttl": 3600}`.
`claude-code resolve --secret --profile <p>` is the `apiKeyHelper` Claude Code calls, so no key lands in
`settings.json`; without `--secret` it prints `<redacted: N bytes>` and, to a pipe, exits 1.

```bash
claude-code vault set [KEY]            # bare on a terminal = guided dialog
claude-code vault get KEY... [--secret [--export]]   # redacted without --secret (main session only)
claude-code vault import keys.env      # then delete the plaintext file
claude-code vault get --secret KEY... | ssh box claude-code vault import - --yes
claude-code doctor --scan|--git|--secrets   # leak audits; never print values
```

**Databricks:** an `oauth` profile seals refresh + access tokens together (`DATABRICKS_<PROFILE>_OAUTH`); `pat` is
sealed too; `m2m` tokens are minted per use, the service-principal secret stays sealed (`DATABRICKS_<PROFILE>_M2M`).
`claude-code databricks token [--profile <id>] [--auth oauth|m2m|pat] [--copy] [--clear-after 30s]` prints type,
length and expiry, never the token; `--copy` clears the clipboard after 30s (kept out of Win+V history on WSL; macOS
clipboard managers and a killed clear process can still keep it). `configure` → databricks offers Set up (OAuth
sign-in, verify, apply) / Login / Paste PAT / Token / Copy token / Apply to agents / Identity.

**The vault key** — 32 random bytes from `HARNESS_VAULT_KEY` → OS keyring (service `claude-code`) → `key_command` (a
JSON argv, no shell). No key file is written; headless boxes set one of the first or last; `HARNESS_KEYRING=off`
skips the keyring. `HARNESS_VAULT_KEY` is moved out of every `claude-code` process's env at start
(`vault.AdoptEnvKey`), so no child inherits it; only the launched Claude Code session gets it.
`vault rotate-secret` re-seals and keeps old keys.

### Environment overrides

Process env read directly by the harness (not pins; set them in the shell or the vault's `env`):

| Variable | Effect |
| --- | --- |
| `HARNESS_KEEP_ENV` | comma-separated names an agent CLI child keeps; codex, copilot, gemini and opencode otherwise drop every inherited `*_KEY`, `*_TOKEN`, `*_SECRET`, `*_PASSWORD` and `*_CREDENTIALS` name except their own login (launch values are added after the scrub) |
| `COWORKER_OVERLAY` | the whole `--overlay` dispatch document (routing keys plus the env layer; `rules/harness/agent.md`): a file path (regular, yours, not group/world-writable, <=64 KiB), inline JSON `'{...}'` or `-` (stdin), applied to the launched child only (one hop: a nested dispatch inherits it cut to `provider`/`profile`, its confinement); `set` secrets only as `@secret:<NAME>`; refused env names: see `rules/harness/agent.md` |
| `HARNESS_KEYRING=off` | skip the OS keyring when finding the vault key |
| `CLAUDE_RECALL_SEMANTIC=0` | memory recall ranks by keyword only (no embeddings) |
| `OPENCODE_DB` | opencode's database: a file name under its data dir, an absolute path, or `:memory:` |
| `DATABRICKS_CONFIG_FILE` | Databricks CLI config file (default `~/.databrickscfg`) |
| `CLAUDE_WL_PASTE_PROXY` | `wl-paste` proxy mode: `0` off, `1` intercept, `reject`; unset = automatic |
| `AGENT_ERROR_FALLBACK` | one of none, pin, agents, all (`1`/`true`/`yes` = all): how a failed dispatch falls back; unset = `none` (exit on the error); process env beats the vault env |
| `HARNESS_EGRESS_HOST` | set by `--remote <h>`: Databricks workspace requests leave through that ssh host |
| `HARNESS_REMOTE_BIN` | the remote `claude-code` binary for `--host` runs |
| `CLAUDE_REMOTE_HOST` | a comma-separated list; the first entry is the machine `--host auto` and `--remote auto` name (`rules/harness/network.md`) |
| `NETWORK_MEMORY` | off by default: unset or any value other than `on` means no sync or connection; `on` (case-insensitive) opts this machine in to memory sync with the hosts in `CLAUDE_REMOTE_HOST` |
| `HARNESS_ALLOW_OPENCODE_2X` | any value lets one install link an opencode newer than 1.x |
| `HARNESS_MODEL_CACHE_TTL` | Go duration a saved discovery snapshot stays fresh for launches and copilot (default `24h`) |
| `HARNESS_USAGE_CACHE_TTL` | seconds a usage fetch stays fresh (default 60; 0 = always refetch) |
| `HARNESS_USAGE_WARN_PCT` | quota warning threshold in percent, `(0,100]` (default 80) |

## Naming

| Term | Means | Values |
| --- | --- | --- |
| agent | the CLI doing the work | `claude` `codex` `copilot` `opencode` `gemini` |
| provider | the platform hosting the endpoint | `anthropic` `databricks` `github` `openai` `google` |
| profile | an account on a provider; carries billing | unique across all providers, `[A-Za-z0-9_-]`, opaque to code |
| auth | how a profile proves who it is | `oauth` (user login) `m2m` (service principal) `pat` `key` `cli` (agent's own login) `none` |
| model | what `--model` takes | an alias (`opus`, follows the table), a version (`opus-5-5`, pinned), a full wire id or a bare served endpoint name (`gpt-6-1-terra`) |
| proxy · router | the loopback daemon · the agent + model pick | `gateway proxy` · `gateway` |
| wire | the protocol a model rides (`providers.WireFor`): Claude → `anthropic`, Responses-only → `responses`, else `openai` | `anthropic` `openai` `responses` |
| default · agent record | what the harness picks when nothing is set · the user's own choice, saved in the vault | flag beats session profile beats agent record beats default; no profile = off, claude always on |

Retired words: runner and vendor (→ agent), router for the daemon (→ proxy), slot and tier (→ model alias; "tier
bucket" is the exception: an alias's ordered Databricks list in `internal/router/table.json`), leg (→ dispatch),
family (→ provider), pin, preference and saved override (→ agent record; the `models --pin` flag keeps its name),
first-party (→ Anthropic, or the agent's own login). `gateway` names the verb namespace only; the Databricks wire is
`serving` (`ai-gateway` is refused).

| Surface | Form | Example |
| --- | --- | --- |
| Label (display) | `<agent> → <provider>·<profile>/<model>` | `copilot → databricks·dev/databricks-claude-opus-5-5` |
| Agent record (vault) | one per agent: profile, model, effort, context, account, aliases | `vault ls` AGENT table, `configure` agent pages |
| Vault secret | the tree path flattened, stopping at auth; a profile named after its provider is written once | `DATABRICKS_PROD_OAUTH`, `ANTHROPIC_KEY` |

Agent records are read from the vault, never inherited env; vendor-read env (`ANTHROPIC_*`, `CLAUDE_CODE_*`,
`COPILOT_*`) is only exported at launch, never renamed; model ids are built per provider from the canonical
form (`opus-5-5` → `claude-opus-5-5`, `databricks-claude-opus-5-5`, `system.ai.claude-opus-5-5`); code never
parses a profile name.

| Provider | Profile | Auth | Vault entry |
| --- | --- | --- | --- |
| anthropic | `ultra` · `anthropic` | `cli` · `key` | `CLAUDE_CODE_OAUTH_TOKEN` (optional, headless Max) · `ANTHROPIC_KEY` |
| databricks | `dev`, `qa`, `prod` · service principals | `oauth` · `m2m` | `DATABRICKS_<PROFILE>_OAUTH` · `DATABRICKS_<PROFILE>_M2M` |
| github | `copilot` (own login), one row per GitHub username | `cli` (gh / `copilot login`) | `COPILOT_GITHUB_TOKEN` (optional; beats gh's active account) |
| openai | `chatgpt` · `openai` | `cli` · `key` | none · `OPENAI_KEY` |
| google | `antigravity` | `cli` | none |

opencode's go/zen accounts stay outside the tree for now (`OPENCODE_API_KEY`).

## Routing

`claude` launches **direct** when the provider speaks `anthropic`, every model alias stays on it, and no `fallback` is
set. Otherwise it goes through the **loopback proxy** (`127.0.0.1:18765`, started on demand), which:

1. requires a per-install client secret (vault `CLAUDE_CODE_ROUTER_SECRET`; `resolve --secret --proxy` prints a scoped
   session token, never the secret) and a loopback `Host`;
2. resolves `model` (`id` or `provider:model`) and strips `[1m]`;
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
claude-code gateway [--model M] [--task "<text>"]   # agent + model pick
```

## Sync the Databricks model menu

Menu sync is explicit and repeatable, never a launch-time network call. Select a **serving** profile; sync uses
its existing authentication and **Serving Endpoints only**, rejecting non-serving and non-Databricks profiles
before requests. `--profile` is required (no pin fallback), and positional arguments are not accepted.

A copilot registry launch does touch the network: it validates identity on every launch, then uses the sealed
snapshot for 24h. It lists live on a miss, a stale snapshot, or a model the snapshot doesn't have.

```bash
claude-code opencode catalog sync --profile dev             # authenticated cache reuse, or discover on miss
claude-code opencode catalog sync --profile dev --dry-run   # explicit preview; no discovery/menu writes
claude-code opencode catalog sync --profile dev --refresh   # force discovery; no stale fallback on failure
claude-code opencode catalog sync --profile dev --verify    # probe eligible endpoints before previewing additions
claude-code opencode catalog sync --profile dev --verify --apply  # seal snapshot/results and add non-failures
claude-code opencode catalog check --profile dev --all      # existing tiny inference/tool probes
```

Every sync first validates the current credential via the workspace's SCIM `Me` endpoint (HTTP 200, nonempty
identity ID, not explicitly inactive). Token presence or expiry alone is not accepted as proof. This establishes
**valid identity, not endpoint permissions**. A refreshed token for the same identity can reuse discovery.

Discovery and sanitized probe outcomes are **encrypted vault secrets**, separate from the plaintext menu overlay.
The `model-discovery/v1/<sha256 JSON scope>` key covers the resolved profile name, CLI profile, normalized workspace
host, serving spelling rule, resolved Anthropic/chat/Responses route URLs and authenticated subject ID.
Changed route URLs cannot reuse old discovery/probe snapshots. No token, token fingerprint, raw probe error or secret
prefix is stored in the snapshot. `catalog sync` has **no TTL**: use `--refresh` when the serving catalog
changes. An unlocked vault key is required; vault/decryption and authentication errors fail closed, even with
`--refresh`. Refresh can
replace invalid snapshot JSON/schema only after successful decryption. It cannot overwrite inaccessible ciphertext.
A successful refresh retains readable historical probe results until
`--verify` replaces them.

Preview/`--dry-run` never saves discovery, verification results or menu changes; normal authentication may refresh
its own credentials. `--apply` (exclusive with `--dry-run`) seals a new/refreshed or freshly verified snapshot even
with zero menu additions. An unchanged cache hit is not rewritten. Launches consume the existing saved menu;
they do not perform discovery or identity validation for this cache.

Sync selects non-gateway `READY` endpoints with task `llm/v1/chat` and a `databricks-` name whose canonical menu ID
round-trips to the exact serving name. It deduplicates and sorts eligible IDs, then appends only absent entries.
Existing entries retain their order, labels and explicit Responses flags, even when absent from discovery;
new entries use the catalog's inferred Responses flag. Chat-only ids never use Responses, and a flag that matches
the catalog is not stored. Nothing is removed. Failed probe outcomes block new
additions, including cached outcomes explicitly labeled **historical**, not live. Listing-only models without a
known failed check may be added as **discovered/unverified**. Empty discovery and no-change runs do not write the
menu. Discovery/authentication failures do not save a snapshot or change the menu.

`--verify` uses existing menu Responses flags and catalog inference for new models, following the clients' resolved
routes: Responses entries use `/responses`, otherwise Claude uses the native Anthropic route's `/v1/messages` when
present, and other models use the chat route's `/chat/completions`. No route is replaced with a hardcoded base.
Resolved routes must stay under the same workspace's HTTPS `/serving-endpoints` path; redirects are rejected.
Verification uses four workers, 30-second request timeouts, a five-minute overall bound and token allowances of
256 for chat, 128 for Responses and 32 for Anthropic. HTTP rejection retries without tools. Success means
HTTP/tool-definition acceptance and a stream that ends cleanly, **not an actual tool call or a validated generated
answer**. A failed probe can
reflect token budget or API-selection limitations rather than an unusable endpoint; no model-specific exceptions are
applied.
After separate successful manual verification, `catalog add --responses` can explicitly select Responses.
Sync seals only status, HTTP code, tool-definition acceptance and latency per model, never raw error text;
the existing `catalog check` command keeps its legacy routes, 8/16-token budgets and result storage.

The vault menu overlay is **shared across profiles**, not scoped to `dev`: OpenCode uses all entries, while Codex
only includes Responses entries with matching bundled model metadata. Relaunch `claude-code opencode` and/or
`claude-code codex` after applying. Listing an endpoint is **not proof of inference access or tool support**;
use `check --all` for verification. Gateway listings are not used as evidence of Serving Endpoints availability.

## Databricks read-only API and cost

**The `databricks api` verb.** `claude-code databricks api <path> --profile <dev|qa|prod> [--as user|sp]` is an
authenticated GET, so no one reaches for `curl` with a token. Databricks profiles only. The path must be plain: no
scheme, host, escapes, `..` or `//`; a query may hold only letters, digits and `_=&.,-`. No redirect is followed, the
body is capped at 8 MiB and the token is never printed. 429, 502, 503 and 504 are retried up to 3 times. Exit 2 on a
refused path, a non-Databricks profile or a bad `--as`; exit 1 on a non-2xx reply. Allowed prefixes (metadata only),
each followed by at most the shown number of extra path segments:

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
claude-code configure provider --list dev   # shows options.warehouse_id and each price
claude-code usage --cost --days 7           # estimate per profile and model
```

`warehouse_id` is 1-64 letters and digits; rates are finite and not negative. `usage --cost` prints a table labelled
`est.` (local ledger tokens times the price table, 1-hour cache writes counted as cache writes; not an invoice), `-`
for a model with no price, and one hint line naming `options.prices` when no profile has prices. Doctor's
`cost report` row passes when nothing is set and warns on a half-set profile. A billed report from `system.billing` is
not available yet: it needs `USE SCHEMA` and `SELECT` on `system.billing` and `system.serving` for the identity that
runs the query. All three workspaces share one metastore, so one warehouse can serve it.
