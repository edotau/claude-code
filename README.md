# claude-code

A minimal, standalone harness for [Claude Code](https://docs.anthropic.com/en/docs/claude-code). This
repo **is** `~/.claude`: one Go binary, `claude-code`, plus the agents, slash commands, rules, skills and
Workflow scripts every session loads.

What the binary adds to a stock install:

- **Provider registry** — Anthropic, a Claude subscription, ChatGPT and GitHub Copilot subscriptions (via their own CLIs), OpenAI, Gemini, Nvidia, Ollama, or any
  gateway, selected per launch or pinned; tier slots (opus/sonnet/haiku/fable) map to model ids per provider.
- **Loopback router** — lets Claude Code talk to OpenAI-dialect upstreams (translation), mix providers per
  tier (`haiku=openai:gpt-5-nano`), and fail over on 429/5xx.
- **Agent runners** — one-shot tasks on Claude, Codex, Gemini, OpenCode or Copilot (`claude-code ask`), and
  interactive vendor CLIs wired to the same registry (`claude-code run`).
- **Hooks + status line** — a Bash safety gate, a context checkpoint, format-on-stop.

## Install

```bash
git clone https://github.com/edotau/claude-code ~/.claude   # or into an existing ~/.claude
cd ~/.claude
make install                        # builds bin/claude-code, links shims + ~/.local/bin/claude-code, renders settings.json
claude-code doctor
export PATH="$HOME/.claude/bin:$PATH"   # optional, in your shell rc: bare claude/codex/... go through the shims
```

`make install` runs `claude-code install`: it copies the binary into `~/.claude/bin`, symlinks the shims
`claude codex gemini opencode copilot` → `claude-code`, links `~/.local/bin/claude-code` (only that name, so
the bare CLIs stay untouched unless you add `~/.claude/bin` to `PATH`), and merges the harness hooks/permissions/statusLine
into `settings.json` after a timestamped backup (foreign hooks are preserved). Preview with
`claude-code install --dry-run`; print the render alone with `claude-code generate` (alias: `settings`).

With `~/.claude/bin` first on `PATH`, `claude` launches Claude Code on the selected provider and
`codex`/`gemini`/`opencode`/`copilot` launch those CLIs through the registry. The real binaries are found
past the shims.

`claude-code` with no arguments prints the menu.

![claude-code help menu](docs/images/cli-menu.svg)

The first word picks what to launch:

```bash
claude-code opus        # Claude Code, opus as the main model (also sonnet | haiku | fable); args pass to claude
claude-code codex       # a vendor CLI through the registry (also gemini | opencode | copilot)
claude-code ollama      # Claude Code on that registered provider; a verb wins a name clash (gemini is the CLI)
claude-code --resume    # a leading flag is a claude flag: Claude Code on the session provider
```

A tier word sets only the main-loop model; subagent slots keep their own pins.

`make bootstrap` is the fresh-box path: it installs the Go toolchain from go.mod's version into `~/.local/go`
when none is on `PATH` (`make go-toolchain`; no sudo), runs `make install`, installs bash tab-completion
(`claude-code completion --install`; `source <(claude-code completion)` for the current shell) and then
`claude-code configure`. `make setup-env APPLY=1` puts `~/.local/go/bin` and `~/go/bin` on your shell's
`PATH` alongside the shims. `make help` lists every target; `make audit` is the CI gate.

## Setup

```bash
claude-code configure                 # on a terminal: the agents menu (one row per agent → its submenu → Submit); piped: every section in order
claude-code configure models          # one section (provider|models|agents|router|github|remote|browser|doctor); --agent codex narrows the menu to that row
claude-code configure --check         # report only, exit 1 if anything is unset (CI / piped)
claude-code generate                  # preview settings.json + every vendor config; --out writes, --check reports drift
claude-code vscode [--check|--dry-run] # merge the first-party settings + keybindings templates into VS Code
```

`configure` and `doctor` probe every stored key with one catalog GET, so a key that is present but rejected
(401) reads `REJECTED`, not `present`. For an absent or rejected credential the TTY dialog offers every way to
fix it side by side — the vendor's browser/device login (`claude auth login`, `codex login`, `copilot login`,
`opencode auth login`, `agy`) and pasting an API key (opening the provider's key console first, if you want).
Keys entered in the menu are staged: the last row, **Submit** `[y/N]`, stores them in the OS keyring
(`env.d/secrets.env` without one); `n` or Esc leaves and discards them. Piped, each option is one `[Y/n]`.

The menu's **claude** row (also `configure provider`) is one sub-picker for every backend the agent can run a
session on: **Subscription** (claude.ai OAuth login, plan usage limits, Remote Control works — pins
`subscription`), **API key** (`ANTHROPIC_API_KEY`, pay per token, no Remote Control — pins `anthropic`), then the
other registered backends (`chatgpt`, `gemini`, `github-copilot`) with their credential state, then **Login** /
**OAuth token** once logged in. Picking a backend resolves a missing credential and pins it. The setup-token is
injected only into `claude -p` launches, so an interactive session's Remote Control uses the `/login` credential.

The Claude **Max plan** has no API key: `claude auth login` is the interactive session (what `claude` uses), and
`claude setup-token` mints a long-lived OAuth token for unattended runs. The claude submenu reports both states
(`claude.ai login (max); headless token set`), offers to mint the token when it is missing, captures it from the
CLI's output and stores it as `CLAUDE_CODE_OAUTH_TOKEN` in `env.d/secrets.env`; the launcher, `ask --agent claude`
and the memory harvest inject it so they keep working after the /login session expires. It is a Claude Code
credential only — it does not authenticate raw Anthropic API calls (that is `ANTHROPIC_API_KEY`). A vendor running on its own
login or config (`codex login`, `opencode auth login`, a `model` in `opencode.jsonc`) counts as configured.
`configure` detects what is already there before asking: a key in the process env, `env.d/secrets.env` or the
OS keyring, or the vendor's own login (Claude subscription, `codex login`, `gh auth`, gemini's Google auth) is
reported as present and reused. Only an absent credential prompts — on a terminal the vendor's own login is
offered and run attached, an API key is read with echo off into the OS keyring (`env.d/secrets.env` without one); piped, every repair
prints the exact command and the run continues. The vendor verbs do the same on demand: `claude-code codex`
first falls back to codex's own ChatGPT login, then offers `configure` for codex, and only then runs the plain
`codex` CLI untouched (so `agy`'s own Google login always works; opencode's own `opencode auth login` store is a leg too) — the harness never ends a vendor launch on a bare
error while someone is at the terminal.

`generate` renders what `install` renders plus each vendor's config from the registry — codex
`~/.codex/config.toml`, opencode `~/.config/opencode/opencode.json` (with your Claude permissions and MCPs
imported), gemini `~/.gemini/antigravity-cli/settings.json` (never over a cached Google login), copilot reported as launch-time env —
as banner-marked blocks with foreign keys untouched and one `.backup` per file. The rendered codex and opencode
blocks reference the key by env var (`HARNESS_CODEX_API_KEY`, `HARNESS_OPENCODE_API_KEY`), which only the shims
export: run `codex`/`opencode` through `~/.claude/bin` (or `claude-code run <vendor>`), not the bare binary, or
the block resolves to no key. `vscode` does the same for VS
Code's Machine settings (`~/.vscode-server/data/Machine/settings.json` on WSL): the Claude Code, Codex and
Copilot extensions pointed at the shims, the terminal profile, right-click = paste in the integrated terminal
(the Windows Terminal habit, on every OS), `--dest` to retarget; that scope is not Settings-Synced, so each
box renders its own. Outside VS Code the same habit is the emulator's own setting: iTerm2 → Preferences →
Pointer → "Right button single click" → Paste from clipboard; Terminal.app has no right-click paste.

### Setup modes: adopt, nuke, workspace

`git clone` refuses a non-empty `~/.claude`; `claude-code install --setup` is the one entry point for that and two
other migrations — `adopt` backs up colliding tracked paths (never `secrets.env`) then restores the
index; `nuke --apply` (typed-path confirm) archives untracked content by name, never value, before
removing the dir; `workspace` scopes a project's own `.claude/`, global harness untouched.

```bash
scripts/bootstrap.sh                                           # fresh box: clone, build, adopt, install
claude-code install --setup adopt --from <clone> --into ~/.claude [--dry-run]
claude-code install --setup nuke [--apply] [--keep-memory] [--account] [--cli]
claude-code install --setup workspace init|link|status
```

## Providers

Built-ins live in `internal/providers/defaults.json`. `~/.claude/env.d/providers.json` overlays them **by name**
(a provider entry replaces the built-in; `"name": null` removes one; `default`/`fallback` override when
set). Start from [`env.d/providers.example.json`](env.d/providers.example.json).

```jsonc
{
  "default": "gateway",
  "fallback": ["gateway", "anthropic"],         // 429/5xx ladder (implies the router)
  "providers": {
    "gateway": {
      "kind": "anthropic",                       // dialect base_url itself speaks
      "base_url": "https://llm.example.com",
      "routes": {                                // extra dialects: path suffix or absolute URL
        "openai": "/v1"
      },
      "auth": {"type": "bearer", "env": "GATEWAY_API_KEY"},
      "headers": {"x-team": "platform"},
      "models": {"opus": "claude-opus-5", "sonnet": "claude-sonnet-5", "haiku": "gpt-5-nano"},
      "one_m": true                              // upstream serves 1M context for Claude ids
    }
  }
}
```

| Field | Meaning |
| --- | --- |
| `kind` | Dialect `base_url` speaks: `anthropic` (`/v1/messages`), `openai` (`/chat/completions`), `responses`, `gemini` |
| `routes` | More dialects on the same gateway — a path joined to `base_url`, or an absolute URL |
| `auth.type` | `x-api-key` · `bearer` · `none` (local servers) · `passthrough` (Claude subscription OAuth) |
| `auth.login` | passthrough only: the vendor CLI whose own sign-in serves the provider — `chatgpt` → `codex login` (ChatGPT plan), `github-copilot` → `copilot login` / `GH_TOKEN` (Copilot plan); `claude-code run codex --provider chatgpt` · `ask --agent copilot --provider github-copilot`. Never routed or used by Claude Code. |
| `models` | slot → model id |
| `headers` | Sent on every upstream request |
| `one_m` | Adds the `[1m]` marker to Claude ids |

```bash
claude-code profile                        # * = default, credential state, dialects, fallback ladder (alias: providers)
claude-code profile gateway                # routes, auth, models
claude-code switch gateway                 # pin the session provider (env.d/provider.env) (alias: use)
claude-code models                         # resolved slots and whether the router is needed
claude-code models --pin haiku=openai:gpt-5-nano --pin opus=claude-opus-5
claude-code models --unpin
claude-code models --live --provider gateway      # what the upstream serves
claude --provider gateway --model claude-sonnet-5                # this launch only
```

Selection order — provider: `--provider` → `HARNESS_PROVIDER` → registry `default`. Slot:
`HARNESS_<SLOT>_MODEL` → `HARNESS_MODEL` → the provider's `models` table. Process env beats
`env.d/provider.env`. A pin of the form `provider:model` crosses providers for that slot.

## Credentials

A provider's credential resolves through a ladder; the first non-empty source wins:

1. **Process env** — `auth.env` (e.g. `GATEWAY_API_KEY`)
2. **`~/.claude/env.d/secrets.env`** — `KEY=value` lines for the same name
   ([`env.d/secrets.env.example`](env.d/secrets.env.example); chmod 600; gitignored)
3. **`auth.file`** — a file holding the credential (`~` expanded)
4. **`auth.command`** — a shell command that prints it, cached for `auth.ttl` seconds (default 300) and
   re-run after an upstream 401:

```json
"auth": {"type": "bearer", "command": "security find-generic-password -w -s gateway", "ttl": 3600}
```

Other commands that fit: `op read op://Private/gateway/credential`, `pass show gateway`, or a
script that mints a short-lived gateway token. `claude-code token --provider <p>` prints the resolved
credential — it is the `apiKeyHelper` Claude Code calls, so no key is stored in `settings.json`.

`claude-code secrets` owns two planes, read in the order env → OS keyring (macOS Keychain / libsecret) →
`env.d/secrets.env`: `secrets set <KEY>` reads the value on stdin and stores it in the keyring, dropping any
plaintext line (`--file`, or a box with no keyring, writes `env.d/secrets.env` at 0600 instead); bare
`secrets set` on a terminal opens a dialog over every tracked secret — move a plaintext one into the keyring,
mint the setup-token, paste a key (its console page opened), delete, or add a custom `NAME` whose pasted
`NAME=value`/`export NAME="value"` line is parsed and shown only redacted; `secrets get <KEY>` reads it back
(`--export` prints an eval-able line); `secrets status` reports which provider is served by which source
without ever printing a value; `secrets scan` and `secrets git` audit the config surface and git history for a
leaked credential and exit 1 on a finding; `secrets doctor` checks perms and key names; `secrets all` runs
every audit. `/harness:secrets-scan` wraps the audits.

## Routing

`claude` launches **direct** when the provider has an `anthropic` route, every slot stays on that provider,
and no `fallback` is configured: `ANTHROPIC_BASE_URL` points at the route and `apiKeyHelper` is
`claude-code token --provider <p>` (omitted for `passthrough`, so subscription login keeps working).
A `passthrough` login also launches direct when only a `fallback` ladder would route it — the loopback
`ANTHROPIC_BASE_URL` disables Remote Control; `claude --router` forces the router for one launch.

Otherwise it launches through the **loopback router** (started on demand, `127.0.0.1:18765` by default):
`ANTHROPIC_BASE_URL=http://127.0.0.1:<port>/p/<provider>` and `apiKeyHelper` = `claude-code token --router`,
a per-install client secret the router requires. Per request the router:

1. checks the client secret and a loopback `Host`;
2. resolves `model` — a plain id on the session provider, or `provider:model` — and strips `[1m]`;
3. `anthropic` route → byte passthrough with the upstream credential injected; `openai` route →
   Anthropic Messages ⇄ chat-completions translation, including streaming and tool calls
   (`count_tokens` is answered locally);
4. on 401/403 refreshes the credential and replays once;
5. on 429/5xx/connect error **before any byte reaches the client**, moves to the next provider in
   `fallback`, mapping the model to the same slot there (429s honor `Retry-After` as a cooldown);
6. on a **hard-quota 429** (the key cannot use that model at all — Gemini free tier's `limit: 0`, OpenAI's
   `insufficient_quota`) demotes to the same provider's next slot (opus → sonnet → haiku) before leaving the
   provider, and remembers the dead model for 30 minutes so later requests skip it. `claude-code ask` does the
   same for its runners: a slot that is quota-dead on the key is retried one slot down, announced on stderr.

Upstream dials are guarded: a provider hostname that resolves into the local network (loopback, link-local,
RFC1918) is refused before the connect, so DNS cannot turn a `base_url` into a pivot onto the router itself. A
literal IP or `localhost` in `base_url` (Ollama, a dev gateway) is your own choice and dials unguarded.

```bash
claude-code router start | status | stop
claude-code router serve --port 18765      # foreground, for debugging
claude --router                            # force the router for a direct-capable provider
```

The built-in default is `subscription` with no fallback ladder, so a plain `claude` runs direct. When a ladder or
cross-provider slot puts a subscription session on the router, it sends the router secret in `X-Claude-Code-Router` (via `ANTHROPIC_CUSTOM_HEADERS`, no `apiKeyHelper`) and
keeps its OAuth login in `Authorization`, which the router forwards to `passthrough` providers only. A
`passthrough` slot under an API-key session provider is refused — that session carries no login to forward.

## Tailscale

`claude-code tailscale status|machines|connect|key|exit-node` — stdlib `net/http` JSON against the admin API
plus exec of the installed `tailscale` CLI; nothing in the harness imports `tailscale.com`. Credentials, resolved
through the same env → keyring → `secrets.env` ladder as providers:

- `TAILSCALE_OAUTH_CLIENT_ID` + `TAILSCALE_OAUTH_CLIENT_SECRET` — preferred; mints a short-lived API token.
- `TAILSCALE_API_KEY` — a 90-day personal key, used as a bearer directly.

Either is only needed for `key` (mint an auth key) and `exit-node --approve`; `status`/`machines`/`connect` work
off the installed CLI alone. `make tailscale-cli` installs the client; `claude-code configure` shows login + the
admin credential as one row.

## Worktrees and agent views

```bash
claude-code worktree list [--format json]            # every ~/worktrees/<repo>/* checkout: branch, dirty/merged/stale/orphaned, reopen + cleanup commands
claude-code worktree create <name> [--branch b]      # the same path the worktree-create hook takes (default branch worktree-<name>)
claude-code worktree clean [--dry-run] <name>        # removes only a clean, merged, unlocked worktree; never --force; branch -d when safe
claude-code worktree clean-all [--dry-run|--yes]     # preview, then remove every eligible one
claude-code agents tree [--session id] [--watch]     # sessions → subagents → workflows: journal-backed status, last activity, vendor legs (↳ codex … ⚠ fallback)
claude-code web view <agentId|prefix> [--follow]     # one agent's snapshot (files edited, legs, result); --follow streams its events until it finishes
claude-code dispatch --list --host <h> --status      # tmux pane state per dispatched agent: working | input-needed | ready | unknown | gone
claude-code web browsers                             # per installed browser: extension | cdp | webdriver | gui control paths, and the system default
claude-code web setup --browser edge                 # opens the Claude in Chrome install page in that browser; never writes Claude Code's host manifests
claude-code web search [--query q]                   # Tavily: bare = the stdio MCP server agents register; --query = one-shot
```

`worktree` is the deterministic engine behind the `worktree-create`/`worktree-remove` hooks and `/workflow:worktree`:
inspection failures read as problems, never as "clean", and `clean` rechecks live state before each removal.
`agents tree`, `web view`, the `claude-code web` dashboard (`/api/tree`, `/api/view`, `/api/browsers`) and the VS Code
"Agents" panel all consume one `agenttree.Build`; a relay agent's vendor run is matched from `ask --log`, so a silent
`codex→claude` fallback is flagged where you look. `web browsers` reports which control path each browser offers —
the user's signed-in session needs `extension` (Chromium family) or `gui`; automation of your own site uses `cdp`
or `webdriver` — and `doctor` / `configure browser` show the default browser's state.

## Agents

```bash
claude-code agents                                   # runners and whether each is available
claude-code ask --agent codex "Review the diff on this branch for correctness bugs."
claude-code ask --log                                 # recent dispatches: id, status, agent, duration, prompt preview
claude-code ask --retry <id> [--timeout 20m]          # replay one exactly; flags typed here override the record
claude-code ask --agent gemini --format json "Map every caller of providers.Select."
claude-code run opencode --provider ollama --model qwen3-coder
claude-code run codex --print-env                    # show the env/argv wiring, launch nothing
claude-code codex ask "Review the diff on this branch for correctness bugs."   # top-level: codex|copilot|opencode
claude-code codex --print-env                         # bare verb = `run codex`; `ask` = one-shot task
claude-code science serve                            # Claude for Life Sciences on its own login, harness env stripped
claude-code usage --days 7 [--workflows]             # subagent tokens (one count per API response) + peak ITPM
claude-code gemini bridge --dirs internal --index "map every caller of providers.Select"  # one 1M-context call
claude-code agents sync --dry-run                    # agents/ → state/projections/<vendor>/agents + vendor homes; skills/ → ~/.agents/skills
```

Exec runners (codex, gemini, opencode) and the interactive `run` launch share one wiring function, so a
gateway route written once serves both. Without `--tools`, claude and opencode get no tools, codex a read-only
sandbox, and gemini its read-only plan mode. The gemini leg runs only on `agy` and its cached Google sign-in
(`~/.gemini/antigravity-cli/antigravity-oauth-token`); there is no API-key or gateway route for it. An interactive
`run opencode` also imports your Claude permissions (`settings.json` allow/deny/ask → opencode's `permission`
block) and MCP servers (`~/.claude.json`, plus project `.mcp.json` entries Claude Code has approved for that
directory — unapproved ones are named in `HARNESS_OPENCODE_MCP_SKIPPED`) and lists every tier model the provider
serves; `--print-env` redacts MCP env, headers, key/token argv and URL queries.

`claude-code gemini` is the gemini agent verb: `gemini ask "<task>"` = `ask --agent gemini` (drives the
Antigravity CLI `agy`); `gemini bridge [--dirs|--files|--index|--diff|--rubric <file>] "<task>"` rides the
subscription via `agy` (default `gemini-3.8-flash-high`, `--model` to override; `--provider` sends it to an
API-key provider instead) — the 1M-context sweep the
`gemini` subagent and `/gemini:gemini` / `/gemini:second-opinion` wrap. Inside a git tree only
tracked/untracked-but-not-ignored files are eligible, and `.env*`, `secrets.env`, `env.d/`, `state/`,
`*.pem`/`*.key` are always skipped; the inlined path list is printed to stderr before the call. Anything else
launches `agy` (same as `run gemini`).
`agents sync` projects `agents/` into every vendor's native subagent format — `~/.codex/agents/<name>.toml`,
`~/.copilot/agents/<name>.md`, `~/.gemini/agents/<name>.md`, `~/.config/opencode/agents/<name>.md` — via a
local rendering under `state/projections/<vendor>/agents/` (gitignored; the home copy is a byte-identical install
of it; `clean --target projections` removes it), and links the
hand-authored `skills/` into `~/.agents/skills`, the one skills dir all four CLIs read natively (so no vendor sees a
skill twice). Every generated file is banner-marked; hand-authored files, symlinks into other trees and real dirs are
kept and reported; `--check` gates drift in both the projections and the homes; `--vendor <v>` narrows a run. `install` runs it
after settings, and also renders CLAUDE.md + the roster into the vendors' global instruction files —
`~/.codex/AGENTS.md` and `~/.gemini/GEMINI.md` (also rendered to `state/projections/<vendor>/…`); nothing is rendered at the harness root,
since Claude Code would load a root AGENTS.md as a second CLAUDE.md in every session (OpenCode reads CLAUDE.md
itself; Copilot has no user-level instruction file). Inside a session, the subagents in `agents/` (`code-workers`,
`copilot`, `gemini`) and the Workflow scripts in `workflows/` handle in-process fan-out; see
`rules/workflow/master-workflow.md`.

## Hooks

Declared in `internal/hookspec/hookspec.go`, implemented in `internal/hooks/`, rendered into
`settings.json` as `$HOME/.claude/bin/claude-code hook <name>`:

| Hook | Event | What it does |
| --- | --- | --- |
| `safety` | PreToolUse (Bash) | Blocks destructive shapes: `rm -rf /`, force-push to main, `curl \| sh`, `sudo`, `DROP TABLE`, `git stash/checkout/reset/restore/clean` aimed at `~/.claude`, … |
| `rtk-wrap` | PreToolUse (Bash) | Routes noisy commands (`git`, `npm`, `cargo`, `kubectl`, `pytest`, `docker`) through the [rtk](https://github.com/rtk-ai/rtk) output compressor when it is on PATH; machine-readable flags (`--porcelain`, `--json`, …) and value subcommands (`rev-parse`, …) bypass it. `HARNESS_RTK_BIN` overrides the binary |
| `docs-placement` | PreToolUse (Write\|Edit) | Blocks a new `.md` at a repo root or under a git-ignored `docs/`, naming the canonical home (`~/.claude/memory/<repo>/reports\|handoffs`, `~/.claude/plans/` for a plan); root anchors and tracked files pass. `HARNESS_DOCS_PLACEMENT=0` disables |
| `agent-effort` | PreToolUse (Agent) | Fills an UNSET subagent model from a local prompt classifier (low→haiku, else sonnet); an explicit `model:` on the dispatch or in the agent's frontmatter always wins |
| `context-checkpoint` | Stop | Once per session at ≥85% context, blocks Stop so the model can hand off before compaction |
| `stop-format` | Stop | Formats files edited this turn: the repo's own `make format\|fmt` target when it honors a per-file variable (`TARGET`/`FILE`/…), else the pyproject-declared python chain, else gofmt/ruff/prettier/shfmt when installed |
| `session-start` | SessionStart | Injects the local memory bank (char-capped); an indexed global bank is a one-line pointer |
| `memory-recall` | UserPromptSubmit | Injects up to 3 bank/native-memory blocks matching ≥2 prompt terms, skipping any already in this session's context (≥4 terms, ≤1,500 chars; else silent) |
| `session-harvest` / `session-harvest-end` | Stop / SessionEnd | After ≥3 edits, spawns a detached `claude-code memory update` that runs `/memory:end`; the first dispatch also claims one `memory watch` per session, which harvests the final state once the transcript has been quiet 10 min (a crash or kill fires no SessionEnd) |
| `access-trace` | PostToolUse (Read) | Logs a model read of a bank file (or its index) to that bank's `access.log`, the signal behind `memory doctor`'s hot/cold nominations |

To add one: implement it in `internal/hooks/`, add a `Spec` to `hookspec.Registry`, `make install`.

## Memory bank

ONE store: `~/.claude/memory/` — a local-only git repo (every settle commits; never pushed; supersede,
never delete). `memory/index.md` is the generated catalog router (one row per repo store + newest global
entries; never hand-edit). The top of the tree is GLOBAL (cross-repo); `memory/<repo>/` is per repo (key =
main-checkout basename; the harness itself is `memory/.claude/`; collisions get a hash suffix; `.origin`
records identity). Each repo dir holds BOTH Claude Code's native auto-memory (`MEMORY.md` + topic files)
and the six typed bank files — `projectContext`, `activeContext`, `progress`, `decisionLog`, `conventions`,
`sessionHistory` (frontmatter `harness_kind: bank`). `MEMORY.md` carries a generated `<!-- bank:begin -->`…
`<!-- bank:end -->` region (≤40 lines/4KB) pointing at the typed files + newest entries; lines outside it are
native/Claude's and are never rewritten. `~/.claude/projects/<slug>/memory` is a harness-maintained symlink
into `memory/<repo>/` (created by `memory migrate`, repaired at SessionStart) — there is no separate
native tree.

What a session pays for: SessionStart injects the catalog (`memory/index.md`) plus this repo's
`activeContext.md` Current Focus/Blockers/Next Steps and `progress.md` In Progress — everything else loads
on demand (`claude-code memory search [--repo <name>|--all]`, Read). `memory-recall` (UserPromptSubmit)
adds up to 3 matching bank/native blocks per prompt, and routes to another repo's store only when the
prompt names it (catalog key or origin basename). Every settle (`memory index`, the background harvest)
keeps it small: `sessionHistory` rotates past 30 entries to `archive/` (in scope — no automatic promotion
to global), the catalog and each touched repo's bank region are regenerated, and the store commits.

```bash
claude-code memory init | path | index [--repo R]   # scaffold, locate, settle + regenerate region/catalog
claude-code memory search [--repo R] <terms> [--k 8] [--format json]  # BM25 over the bank
claude-code memory doctor [--stale-days 90] [--notice]  # cap offenders, staleness
claude-code memory stats | timeline [--repo R] [-n 20] | promote [--repo R]  # stats is store-wide; promote = settle pass
claude-code memory show <id> | digest [--repo R]          # one block by its [id]; haiku digest of the raw archive → archive/digest.md
claude-code memory migrate [--dry-run] [--origin SLUG=PATH]...   # one-time cutover from legacy trees
claude-code memory migrate --manifest MANIFEST                 # apply a reviewed --dry-run manifest (also moves legacy docs/{plans,reports,handoffs,memory/native})
claude-code memory alias --repo CHECKOUT --into KEY [--dry-run]  # fold a second checkout's store into its canonical one
```

`claude-code memory archive [--repo R] [--dupes] [--apply] [--restore] [--split N] [<topic>...]` governs the
native MEMORY.md's size within a repo's dir: `--split N --apply` keeps an N-line core in `MEMORY.md` and
moves older pointer lines to `SEARCHABLE.md` (indexed by recall, never injected); `--dupes` clusters
near-duplicate topics (report-only); `archive <topic> --apply` moves a cold topic out and `--restore`
brings it back. `access.log` beside each store records every recall.
`/memory:init` enriches, `/memory:end` saves manually.

## Housekeeping

- `claude-code clean [--target <targets>] [--older-than 30d] [--list]` — sweeps accumulated state (caches,
  run dirs, backups); applies by default, `--dry-run` previews. Credentials and settings are never a target.
  Opt-in only: `--target projections` (vendor renders) and `--target sessions` (Claude Code transcripts under
  `projects/<slug>/` with their `<id>/` subagent dirs, aged by `--older-than`; `projects/<slug>/memory` stays).
  `--target sessions` always asks before it deletes — a `[y/N]` prompt on a TTY, `--yes` without one.
  Every run also checks for a nested `.claude/` inside the config dir (project scope layered over user
  settings): it goes once user `settings.json` carries all its keys, else clean names the keys to move into the template.
- `claude-code history snapshot|log|diff` — shadow git history of config + memory, kept at
  `state/harness/history`; `env.d/secrets.env` is never snapshotted.
- `claude-code theme [--preview]` — pick the statusline color palette.
- `claude-code doctor --audit [--days 7] [--format json]` — the typed action log: provider switches, model pins, secret sets (name only), safety-hook blocks (reason, never the command), router failovers.
- `claude-code mcp doctor [--format json]` — MCP server connect health, cross-scope drift, inline plaintext secrets.

## Status line

`claude-code statusline [--style gradient|powerline|capsule|minimal|dashboard]`: model, provider (and
cross-provider slot), router state, context %, approximate session tokens, duration, git branch and diff. Persistent choice and
palettes: `~/.claude/statusline/config.json` (+ `palettes.json`).

## Layout

```
cmd/claude-code/     main
internal/            cli (verb table) · providers · router · translate · launch · vendors · agent
                     hooks · hookspec · settings · statusline · models · paths
agents/ commands/ rules/ skills/ workflows/   loaded by Claude Code
env.d/               provider.env (pins) · secrets.env (credentials) — untracked; *.example tracked
plans/               implementation plans — Claude Code's native plansDirectory; untracked, gitignored
```

## Development

```bash
go test ./internal/<pkg>           # while developing — only what you touched
make lint                          # gofmt check
make test                          # go vet + full suite, once, before pushing (= make audit, the CI gate)
```

`git push` runs `make audit` first: `install` sets `core.hooksPath` to `.githooks/`, whose `pre-push` hook is the
same gate `ci.yml` runs, so a red push is caught locally. `git push --no-verify` bypasses it.

`settings.json` is generated — edit `internal/settings/settings.tmpl.json` and re-run `make install`.
The template is the default: keys it sets (model, effort, theme, tui, plugins, `permissions.defaultMode`, …)
are reset on every install, so a `/config` change lasts until then — fold it into the template to keep it. Permission
lists are unioned, so a rule removed from the template must also be removed from `settings.json` by hand.
Install also seeds `~/.tmux.conf` from `internal/launch/tmux.conf` when no tmux config exists; an existing one is kept.
