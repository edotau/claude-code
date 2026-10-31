# claude-code

A minimal harness for [Claude Code](https://docs.anthropic.com/en/docs/claude-code). This repo **is** `~/.claude`:
one Go binary, `claude-code`, plus the agents, commands, rules, skills and Workflow scripts every session loads.

What it adds to a stock install:

- **Provider registry** — Anthropic (Claude login or API key), Databricks, OpenAI (ChatGPT login or API key),
  GitHub Copilot, Google (Antigravity), NVIDIA or any custom endpoint; model aliases (opus/sonnet/haiku/fable) map to
  model ids per profile.
- **Loopback proxy** — OpenAI-dialect translation, per-model-alias provider mixing, failover on 429/5xx.
- **Agents** — one-shot tasks on Claude, Codex, Gemini, OpenCode or Copilot (`ask`), or their interactive CLIs
  (`claude-code <agent>`).
- **Hooks, memory, status line** — a Bash safety gate, a per-repo memory bank, format-on-stop, context checkpoint.

Every verb takes `--help`; `claude-code describe` maps the whole harness. Agent-facing commands and gotchas live
per area under `rules/harness/` and `rules/workflow/` (index: `CLAUDE.md` → How-to).

## Install

```bash
git clone https://github.com/edotau/claude-code ~/.claude
cd ~/.claude
make install                  # build, link agent CLIs into bin/, render settings.json, add linters (DEV_TOOLS=0 skips)
claude-code doctor            # check binaries, providers, credentials, proxy
```

- `make bootstrap` — fresh box: Go toolchain (no sudo), install, bash completion, then `claude-code configure`
  (`CHECK=1` runs `configure --check` instead; `TAILSCALE=1` adds tailscale).
- `claude-code install --dry-run` previews; foreign hooks in `settings.json` are kept, with a timestamped backup.
- A missing agent CLI is reported, never downloaded; `install --tools=<name>` installs it.
- `bin/<agent>` is the real, unrouted CLI. Routed launches go through `claude-code <agent>`.

![claude-code help menu](docs/images/cli-menu.svg)

```bash
claude-code opus        # Claude Code with opus as the main model (also sonnet | haiku | fable)
claude-code codex       # an agent CLI through the registry (also gemini | opencode | copilot)
claude-code qa          # Claude Code on that registry profile
claude-code --resume    # a leading flag passes to claude, on the session provider
```

## Setup

```bash
claude-code configure                 # interactive menu: providers, logins/keys, models, agents; Submit seals keys
claude-code configure models          # one page (provider|models|agents|router|github|remote|browser|claude|codex|routing|databricks|doctor)
claude-code configure claude|codex    # per-machine settings overrides | codex effort and verbosity (vault)
claude-code configure --check         # report only; exit 1 if anything is unset
claude-code configure --restore [--out f|--check|--dry-run]   # render settings + agent CLI configs
claude-code configure --revert [--agent a,b]        # restore agent CLI configs from backup
```

- `configure` reuses what already exists (env, vault, agent CLI logins) and prompts only for what is missing.
- **Claude subscription** keeps Remote Control; **API key** pays per token. For headless runs on a Max plan,
  `configure` mints `claude setup-token` into the vault as `CLAUDE_CODE_OAUTH_TOKEN`.
- Agent CLI configs are merged, never replaced, with 5 backups per file; details: `rules/harness/agent.md`.
- From a worktree, render only that checkout:
  `CLAUDE_CONFIG_DIR="$PWD" claude-code configure --restore --out "$PWD/settings.json"`
  (agent CLI homes are skipped unless `--agents`).

- `~/.claude` already exists: clone into `~/.claude.stage`, then `make -C ~/.claude.stage adopt` (backs up
  collisions, then bootstraps). `install --setup nuke [--apply] [--keep-memory]` archives untracked content and
  removes it; `install --setup workspace init|link|status` scopes a project's own `.claude/`.

## Providers

Built-ins live in `internal/providers/defaults.json`. `claude-code vault edit` overlays them **by name** (an entry
replaces the built-in; `null` removes it). Each entry is a **profile** (an account on a provider: `qa`,
`max`, `chatgpt`); see [Naming](#naming).

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
claude-code models --pin haiku=openai:gpt-5-nano    # pin a model alias; provider:model crosses providers
claude-code models --unpin | --live --profile p     # clear pins | list what upstream serves
claude-code --profile example --model sonnet         # this launch only
```

Precedence: `--profile` → vault pin `CLAUDE_PROFILE` → registry `default`. Model: `--model` → `CLAUDE_<TIER>_MODEL` →
`CLAUDE_MODEL` → the profile's `models`. Process env beats vault pins.

### Vault

`state/vault.db` holds pins, custom launch env, provider/agent overlays and `key_command` as one JSON document, plus
sealed secrets. `vault edit` opens it in `$EDITOR` and saves atomically; secrets never appear in it. `vault list`
shows secrets as `<prefix>***  N chars` and marks failing entries ✗ first (`--format json` keeps empty sections).

```jsonc
{
  "pins": {"CLAUDE_PROFILE": "max", "CLAUDE_HAIKU_MODEL": "openai:gpt-5-nano"},
  "env": {"MY_PROJECT_ROOT": "/home/me/work", "SOME_API_TOKEN": "@secret"},   // @secret = vault secret of that name
  "providers": {"default": "example"}, "vault": {"key_command": ""}
}
```

## Credentials

First non-empty source wins: **process env** (`auth.env`) → **vault** (`claude-code vault set <KEY>`, AES-256-GCM) →
**`auth.file`** → **`auth.command`** (prints the credential; cached `auth.ttl` seconds, default 300, re-run on 401).

Example: `"auth": {"type": "key", "command": "op read op://Private/example/credential", "ttl": 3600}`.
`claude-code resolve --profile <p>` is the `apiKeyHelper` Claude Code calls, so no key lands in `settings.json`.

```bash
claude-code vault set [KEY]            # bare on a terminal = guided dialog
claude-code vault get KEY... [--export]
claude-code vault import keys.env      # then delete the plaintext file
claude-code vault get KEY... | ssh box claude-code vault import - --yes
claude-code doctor --scan|--git|--secrets   # leak audits; never print values
```

**Databricks:** an `oauth` profile seals refresh + access tokens together (`DATABRICKS_<PROFILE>_OAUTH`); `pat` is
sealed too; `m2m` tokens are minted per use, the service-principal secret stays sealed (`DATABRICKS_<PROFILE>_M2M`).
`claude-code databricks mint [--profile <id>] [--copy] [--clear-after 30s]` prints type, length and expiry, never the
token; `--copy` clears the clipboard after 30s (kept out of Win+V history on WSL; macOS clipboard managers and a
killed clear process can still keep it). `configure` → databricks offers Set up (OAuth sign-in, verify, apply) / Login /
Paste PAT / Mint / Apply to agents.

**The vault key** — 32 random bytes from `HARNESS_VAULT_KEY` → OS keyring (service `claude-code`) → `key_command` (a
JSON argv, no shell). No key file is written; headless boxes set one of the first or last. `HARNESS_VAULT_KEY` is
scrubbed from agent CLI children. `vault rotate-secret` re-seals and keeps old keys.

## Naming

| Term | Means | Values |
| --- | --- | --- |
| agent | the CLI doing the work | `claude` `codex` `copilot` `opencode` `gemini` |
| provider | the platform hosting the endpoint | `anthropic` `databricks` `github` `openai` `google` `nvidia` |
| profile | an account on a provider; carries billing | unique across all providers, `[a-z0-9-]`, opaque to code |
| auth | how a profile proves who it is | `oauth` (user login) `m2m` (service principal) `pat` `key` `cli` (agent's own login) `none` |
| model | what `--model` takes | an alias (`opus`, follows the table), a version (`opus-5-5`, pinned) or a full wire id |
| proxy · router | the loopback daemon · the agent + model pick | `gateway proxy` · `gateway router` |
| wire | the protocol a model rides (`providers.WireFor`): Claude → `anthropic`, Responses-only → `responses`, else `openai` | `anthropic` `openai` `responses` |

Retired words: runner and vendor (→ agent), router for the daemon (→ proxy), slot and tier (→ model alias), leg
(→ dispatch), family (→ provider). `gateway` names the verb namespace only; the Databricks wire is `ai-gateway`.

| Surface | Form | Example |
| --- | --- | --- |
| Label (display) | `<agent> → <provider>·<profile>/<model>` | `copilot → databricks·dev/databricks-claude-opus-5-5` |
| Pins (vault only) | `<AGENT>_<FIELD>` | `CLAUDE_PROFILE`, `CLAUDE_OPUS_MODEL`, `COPILOT_PROFILE`, `GEMINI_MODEL` |
| Vault secret | the tree path flattened, stopping at auth; a profile named after its provider is written once | `DATABRICKS_PROD_OAUTH`, `ANTHROPIC_KEY` |

Pins are read from the vault, never inherited env; vendor-read env (`ANTHROPIC_*`, `CLAUDE_CODE_*`, `COPILOT_*`) is
only exported at launch, never renamed; model ids are built per provider from the canonical form (`opus-5-5` →
`claude-opus-5-5`, `databricks-claude-opus-5-5`, `system.ai.claude-opus-5-5`); code never parses a profile name.

| Provider | Profile | Auth | Vault entry |
| --- | --- | --- | --- |
| anthropic | `max` · `anthropic` | `cli` · `key` | `ANTHROPIC_MAX_OAUTH` (optional; exports `CLAUDE_CODE_OAUTH_TOKEN`) · `ANTHROPIC_KEY` |
| databricks | `dev`, `qa`, `prod` · service principals | `oauth` · `m2m` | `DATABRICKS_<PROFILE>_OAUTH` · `DATABRICKS_<PROFILE>_M2M` |
| github | `personal`, `work` | `oauth` | `GITHUB_PERSONAL_OAUTH` (exports `COPILOT_GITHUB_TOKEN`) |
| openai | `chatgpt` · `openai` | `cli` · `key` | none · `OPENAI_KEY` |
| google | `antigravity` | `cli` | none |
| nvidia | `nvidia` | `key` | `NVIDIA_KEY` |

opencode's go/zen accounts stay outside the tree for now (`OPENCODE_API_KEY`).

## Routing

`claude` launches **direct** when the provider speaks `anthropic`, every model alias stays on it, and no `fallback` is
set. Otherwise it goes through the **loopback proxy** (`127.0.0.1:18765`, started on demand), which:

1. requires a per-install client secret (vault `CLAUDE_CODE_ROUTER_SECRET`; `resolve --proxy` prints it) and a
   loopback `Host`;
2. resolves `model` (`id` or `provider:model`) and strips `[1m]`;
3. passes `anthropic` routes through; translates Messages ⇄ chat-completions for `openai` routes (streaming, tools);
4. refreshes the credential and replays once on 401/403 (the error names the remedy for the row's auth type);
5. fails over along `fallback` on 429/5xx/connect error before the first byte;
6. on a hard-quota 429 demotes opus → sonnet → haiku and skips the dead model for 30 minutes;
7. returns an upstream 400 `prompt is too long` as a clean `invalid_request_error`, never retried.

Provider hostnames resolving into the local network are refused (a literal IP or `localhost` is allowed). The proxy
disables Remote Control, so a subscription session stays direct under a `fallback` ladder. Upstream requests carry
`User-Agent: claude-code/<version> <agent>`; `HARNESS_REQUEST_TAGS` (single-line JSON, ≤ 4 KiB) goes out as
`Databricks-Ai-Gateway-Request-Tags` on AI Gateway rows only. `HARNESS_HEADERS` (one `Name: value` per line, ≤ 4 KiB)
adds upstream headers; invalid lines are dropped, and auth, attribution and framing headers cannot be overridden.

```bash
claude-code gateway proxy [--start|--stop|--status]   # bare = --status
claude-code gateway proxy --serve --port 18765        # foreground
claude-code gateway router [--model M] [--task "<text>"]   # agent + model pick
```

## Agents

```bash
claude-code agents                                     # agents and availability
claude-code ask --agent codex "Review this branch."    # one-shot; --format json, --tools, --effort
claude-code ask --agents <json|file> --agent <name> "<task>"   # inline agent definitions
claude-code ask --log | --retry <id>                   # recent dispatches; replay one
claude-code ask --batch tasks.jsonl|- [--jobs N] [--fail-fast]   # many tasks on a pool; one envelope per line
claude-code opencode --profile ollama --model qwen3-coder   # interactive; -- forces passthrough
claude-code opencode catalog [list|add|remove|import|reset|check|sync]   # shared Databricks model menu
claude-code codex review --adversarial --base main     # per-agent verbs; claude-code codex -h lists them
claude-code gemini bridge --dirs internal "<task>"     # one 1M-context sweep via agy
claude-code usage --days 7                             # subagent token usage
```

Without `--tools`, agents are read-only. `agents sync` renders under `state/projections/<agent>/`, installs into
`~/.codex`, `~/.copilot`, `~/.gemini`, `~/.config/opencode`, and links `skills/` into `~/.agents/skills`; no AGENTS.md
lives at this root (Claude Code would load it as a second CLAUDE.md). Envelope fields and per-agent behavior:
`rules/harness/agent.md`; in-session fan-out: `rules/workflow/master-workflow.md`.

### Sync the Databricks model menu

Discovery is explicit and repeatable, never a launch-time network call. Select a **serving** profile; sync uses
its existing authentication and **Serving Endpoints only**, rejecting AI Gateway and non-Databricks profiles
before requests. `--profile` is required (no pin fallback), and positional arguments are not accepted.

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
prefix is stored in the snapshot. There is **no TTL**: use `--refresh` when the serving catalog changes. An unlocked
vault key is required; vault/decryption and authentication errors fail closed, even with `--refresh`. Refresh can
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
new entries use the catalog's inferred Responses flag. Nothing is removed. Failed probe outcomes block new
additions, including cached outcomes explicitly labeled **historical**, not live. Listing-only models without a
known failed check may be added as **discovered/unverified**. Empty discovery and no-change runs do not write the
menu. Discovery/authentication failures do not save a snapshot or change the menu.

`--verify` uses existing menu Responses flags and catalog inference for new models, following the clients' resolved
routes: Responses entries use `/responses`, otherwise Claude uses the native Anthropic route's `/v1/messages` when
present, and other models use the chat route's `/chat/completions`. No route is replaced with a hardcoded base.
Resolved routes must stay under the same workspace's HTTPS `/serving-endpoints` path; redirects are rejected.
Verification uses four workers, 30-second request timeouts, a five-minute overall bound and token allowances of
256 for chat, 128 for Responses and 32 for Anthropic. HTTP rejection retries without tools. Success means
HTTP/tool-definition acceptance, **not an actual tool call or a validated generated answer**. A failed probe can
reflect token budget or API-selection limitations rather than an unusable endpoint; no model-specific exceptions are
applied.
After separate successful manual verification, `catalog add --responses` can explicitly select Responses.
Sync seals only status, HTTP code, tool-definition acceptance and latency per model, never raw error text;
the existing `catalog check` command keeps its legacy routes, 8/16-token budgets and result storage.

The vault menu overlay is **shared across profiles**, not scoped to `dev`: OpenCode uses all entries, while Codex
only includes Responses entries with matching bundled model metadata. Relaunch `claude-code opencode` and/or
`claude-code codex` after applying. Listing an endpoint is **not proof of inference access or tool support**;
use `check --all` for verification. Gateway listings are not used as evidence of Serving Endpoints availability.

### Clipboard over SSH

Image paste in an SSH session reads the client's clipboard, never the WSL host's: the `wl-paste` proxy and
`claude-code clip` go through a forwarded socket (`/tmp/claude-clip-<user>.sock`) to a Mac-side bridge. Two one-time
owner steps:

```text
# Mac ~/.ssh/config
Host <wsl-host>
    RemoteForward /tmp/claude-clip-%r.sock %d/.claude/run/clip.sock

# WSL /etc/ssh/sshd_config (then: sudo systemctl restart ssh)
StreamLocalBindUnlink yes
```

`make install` on the Mac installs an on-demand launchd agent (`clip --serve` on `~/.claude/run/clip.sock`); nothing
runs per session. Diagnose from the SSH session with `claude-code clip` (saves the image, prints its path). Failures:
`no clipboard bridge from the SSH client (<sock>: …)` (forward missing, not a
socket, or "is not owned by you" — a socket another user pre-bound is refused), and `no image in the SSH client's
clipboard — copy one there first (or its bridge is down)`.

## Hooks

Declared as **groups** in `internal/hooks/hookspec/hookspec.go`, implemented in `internal/hooks/`; add one by
registering it in a group and running `make install`. Settings carry one dispatcher entry per event
(`claude-code hook <Event>`). The `safety` group (`safety-scan`: blocks destructive Bash, asks on
intent-dependent commands) keeps its own process and cannot be turned off; any other group is switched off with
`"<group>": false` under `harnessHooks` in the vault.

| Group | Does |
| --- | --- |
| `safety` | Bash safety gate (block / ask / warn tiers) |
| `rtk` | Routes noisy command output through rtk |
| `docs` | Markdown placement gate; plan template, dated plan copies, plan-task ticks |
| `dispatch` | Concurrency cap (12 per account, ≤ 4 × CPUs), subagent model fill, memory pointers on spawn |
| `paths` | Resolves Windows ⇄ `/mnt/…` paths in prompts |
| `memory` | Injects the catalog and recall at start/prompt; context checkpoint and memory update at Stop/SessionEnd/PreCompact |
| `format` | Formats files edited this turn |
| `worktree` | Creates/removes under `~/worktrees/<repo>/`, keeps the ledger |
| `codex` | Codex only: child-agent routing and its log |

Per-hook matrix: `rules/harness/hooks.md`.

## Memory bank

One store, `~/.claude/memory/`: a local-only git repo (every settle commits, never pushed). `index.md` is the
generated catalog (never hand-edit), `home/` the cross-repo bank, `claude-code/` the harness's own, `<repo>/` one per
repo keyed by origin remote basename. Each bank dir holds Claude Code's native memory (`MEMORY.md` + topics) and six
typed files (`projectContext`, `activeContext`, `progress`, `decisionLog`, `conventions`, `sessionHistory`);
`~/.claude/projects/<slug>/memory` symlinks into it. A session gets the catalog plus this repo's active context; the
rest loads on demand. `memory dream` consolidates daily in the background. Search: `claude-code memory recall
[--repo R|--all] <terms>` (BM25). Verbs and file contracts: `rules/workflow/memory-bank.md`.

## Status line

`claude-code theme statusline [--style gradient|powerline|capsule|minimal|dashboard]` shows
`<provider>·<profile> · <model>`, context % and git state, plus the 5h/weekly limits (`5h:N% 7d:N%`) or a minted
token's time left (`oauth:42m`). Opt-in `segments`: `router`, `tokens` (`total:1.23M`, `+` while dispatches run),
`duration`, `session`, `agents` (`N ◉`). Config: `~/.claude/state/statusline/config.json`.

## Layout

`CLAUDE.md` → Where things live. State: `state/vault.db` (untracked), `state/harness/<owner>/` (versioned per the
`internal/schemas` registry; `doctor` reports `formats` and unclaimed `state layout` entries, `make install` runs
pending migrations), `state/harness/backups/` (every plaintext backup, via `paths.Backup`); `docs/templates/` indexes
every embedded (`//go:embed`) template.

## Command reference

| To… | Run |
| --- | --- |
| Adopt, wipe or scope a `.claude/` | `claude-code install --setup adopt\|nuke\|workspace` |
| Bash tab completion | `claude-code completion --install` |
| OpenCode reasoning effort | TUI `ctrl+t` · `opencode run --variant high` · `claude-code ask --effort high` |
| Worktrees under `~/worktrees/<repo>/` | `claude-code worktree list\|create <n>\|merge <n>\|clean <n>\|clean-all` |
| Subagent status and dispatches | `claude-code agents tree [--watch\|--background]` · `agents reap [--dry-run]` · `network view <id> [--follow]` |
| Run an agent on another tailnet box | `claude-code dispatch --host <h> --agent <a> "<task>"` · `--list --status` |
| Watch agents from a phone | `claude-code network page [--addr ip:port]` |
| Browsers, scraping, web search | `claude-code network browser [--setup --browser <b>]` · `--extract <url> …` · `network websearch --query q` |
| Tailnet | `claude-code network tailscale [--machines\|--connect\|--key\|--exit-node …\|--subnet <cidr> …]` (`TAILSCALE_OAUTH_CLIENT_ID`/`_SECRET` or `TAILSCALE_API_KEY` in the vault; `make tailscale-cli` installs it) |
| VS Code on your phone | `claude-code network vscode [--stop\|--status] [--funnel]` (token-gated URL + QR) |
| Keep MEMORY.md small | `claude-code memory archive [--split 20 --apply]` |
| Lint a workflow script | `claude-code workflow lint` · `workflow skeleton <pattern>` |
| Housekeeping | `claude-code clean [--list\|--dry-run]` · `doctor --audit [--days 7]` · `history snapshot\|log\|diff` · `network mcp` |
| Theme, VS Code | `claude-code theme [--preview] [<palette>]` · `theme apply` · `theme vscode [--check\|--dry-run]` |

## Development

```bash
go test ./internal/<pkg>   # while developing — only what you touched
make lint                  # fmt, vet, shell, yaml, markdown (CHECK=1 reports only)
make lint AGENT=on         # auto-fix, then Claude fixes the rest (HARNESS_LINT_AGENT=on sets the default)
make test                  # full gate, once before pushing (= make audit = CI)
```

- `git push` runs the same gate via `.githooks/pre-push`; `--no-verify` bypasses it.
- `settings.json` is generated: edit `defaults/claude/settings.json` (or `claude-code configure` → "Save to repo
  defaults"), then `make install`. Default keys reset on every install; permission lists are unioned, so a removed
  rule must also be deleted from `settings.json` by hand.
