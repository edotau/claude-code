# claude-code

A minimal, standalone harness for [Claude Code](https://docs.anthropic.com/en/docs/claude-code). This
repo **is** `~/.claude`: one Go binary, `claude-code`, plus the agents, slash commands, rules, skills and
Workflow scripts every session loads.

What the binary adds to a stock install:

- **Provider registry** — Anthropic, a Claude subscription, ChatGPT and GitHub Copilot subscriptions (via their own CLIs), OpenRouter, OpenAI, Gemini, Ollama, or any
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
claude-code openrouter  # Claude Code on that registered provider; a verb wins a name clash (gemini is the CLI)
claude-code --resume    # a leading flag is a claude flag: Claude Code on the session provider
```

A tier word sets only the main-loop model; subagent slots keep their own pins.

`make bootstrap` is the fresh-box path: it checks `go`, runs `make install`, installs bash tab-completion
(`claude-code completion --install`; `source <(claude-code completion)` for the current shell) and then
`claude-code configure`. `make help` lists every target; `make audit` is the CI gate.

## Setup

```bash
claude-code configure                 # every section, in order: provider → keys → models → agents → router → doctor
claude-code configure keys            # one section; --vendor codex scopes it
claude-code configure --check         # report only, exit 1 if anything is unset (CI / piped)
claude-code generate                  # preview settings.json + every vendor config; --out writes, --check reports drift
claude-code vscode [--check|--dry-run] # merge the first-party VS Code settings template into the Machine settings
```

`configure` detects what is already there before asking: a key in the process env, `env.d/secrets.env` or the
OS keyring, or the vendor's own login (Claude subscription, `codex login`, `gh auth`, gemini's Google auth) is
reported as present and reused. Only an absent credential prompts — on a terminal the vendor's own login is
offered and run attached, an API key is read with echo off into `env.d/secrets.env`; piped, every repair
prints the exact command and the run continues. The vendor verbs do the same on demand: `claude-code codex`
first falls back to codex's own ChatGPT login, then offers `configure` for codex, and only then runs the plain
`codex` CLI untouched (so `gemini auth login` always works; opencode's own `opencode auth login` store is a leg too) — the harness never ends a vendor launch on a bare
error while someone is at the terminal.

`generate` renders what `install` renders plus each vendor's config from the registry — codex
`~/.codex/config.toml`, opencode `~/.config/opencode/opencode.json` (with your Claude permissions and MCPs
imported), gemini `~/.gemini/settings.json` (never over a Google login), copilot reported as launch-time env —
as banner-marked blocks with foreign keys untouched and one `.backup` per file. The rendered codex and opencode
blocks reference the key by env var (`HARNESS_CODEX_API_KEY`, `HARNESS_OPENCODE_API_KEY`), which only the shims
export: run `codex`/`opencode` through `~/.claude/bin` (or `claude-code run <vendor>`), not the bare binary, or
the block resolves to no key. `vscode` does the same for VS
Code's Machine settings (`~/.vscode-server/data/Machine/settings.json` on WSL): the Claude Code, Codex and
Copilot extensions pointed at the shims, the terminal profile, right-click = paste in the integrated terminal
(the Windows Terminal habit, on every OS), `--dest` to retarget; that scope is not Settings-Synced, so each
box renders its own. Outside VS Code the same habit is the emulator's own setting: iTerm2 → Preferences →
Pointer → "Right button single click" → Paste from clipboard; Terminal.app has no right-click paste.

## Providers

Built-ins live in `internal/providers/defaults.json`. `~/.claude/providers.json` overlays them **by name**
(a provider entry replaces the built-in; `"name": null` removes one; `default`/`fallback` override when
set). Start from [`providers.example.json`](providers.example.json).

```jsonc
{
  "default": "litellm",
  "fallback": ["litellm", "openrouter"],        // 429/5xx ladder (implies the router)
  "providers": {
    "litellm": {
      "kind": "anthropic",                       // dialect base_url itself speaks
      "base_url": "https://llm.example.com",
      "routes": {                                // extra dialects: path suffix or absolute URL
        "openai": "/v1"
      },
      "auth": {"type": "bearer", "env": "LITELLM_API_KEY"},
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
claude-code profile litellm                # routes, auth, models
claude-code switch litellm                 # pin the session provider (env.d/provider.env) (alias: use)
claude-code models                         # resolved slots and whether the router is needed
claude-code models --pin haiku=openai:gpt-5-nano --pin opus=claude-opus-5
claude-code models --unpin
claude-code models --live --provider openrouter   # what the upstream serves
claude --provider openrouter --model anthropic/claude-sonnet-5   # this launch only
```

Selection order — provider: `--provider` → `HARNESS_PROVIDER` → registry `default`. Slot:
`HARNESS_<SLOT>_MODEL` → `HARNESS_MODEL` → the provider's `models` table. Process env beats
`env.d/provider.env`. A pin of the form `provider:model` crosses providers for that slot.

## Credentials

A provider's credential resolves through a ladder; the first non-empty source wins:

1. **Process env** — `auth.env` (e.g. `OPENROUTER_API_KEY`)
2. **`~/.claude/env.d/secrets.env`** — `KEY=value` lines for the same name
   ([`env.d/secrets.env.example`](env.d/secrets.env.example); chmod 600; gitignored)
3. **`auth.file`** — a file holding the credential (`~` expanded)
4. **`auth.command`** — a shell command that prints it, cached for `auth.ttl` seconds (default 300) and
   re-run after an upstream 401:

```json
"auth": {"type": "bearer", "command": "security find-generic-password -w -s openrouter", "ttl": 3600}
```

Other commands that fit: `op read op://Private/openrouter/credential`, `pass show openrouter`, or a
script that mints a short-lived gateway token. `claude-code token --provider <p>` prints the resolved
credential — it is the `apiKeyHelper` Claude Code calls, so no key is stored in `settings.json`.

`claude-code secrets` owns the `env.d/secrets.env` plane: `secrets set <KEY>` reads the value on stdin and
writes it at 0600 (`--keyring` stores it in the OS keyring instead); `secrets get <KEY>` reads it back
(`--export` prints an eval-able line); `secrets status` reports which provider is served by which source
without ever printing a value; `secrets scan` and `secrets git` audit the config surface and git history for a
leaked credential and exit 1 on a finding; `secrets doctor` checks perms and key names; `secrets all` runs
every audit. `/harness:secrets-scan` wraps the audits.

## Routing

`claude` launches **direct** when the provider has an `anthropic` route, every slot stays on that provider,
and no `fallback` is configured: `ANTHROPIC_BASE_URL` points at the route and `apiKeyHelper` is
`claude-code token --provider <p>` (omitted for `passthrough`, so subscription login keeps working).

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
   `fallback`, mapping the model to the same slot there (429s honor `Retry-After` as a cooldown).

Upstream dials are guarded: a provider hostname that resolves into the local network (loopback, link-local,
RFC1918) is refused before the connect, so DNS cannot turn a `base_url` into a pivot onto the router itself. A
literal IP or `localhost` in `base_url` (Ollama, a dev gateway) is your own choice and dials unguarded.

```bash
claude-code router start | status | stop
claude-code router serve --port 18765      # foreground, for debugging
claude --router                            # force the router for a direct-capable provider
```

The built-in default is `subscription` with `fallback: ["gemini"]`, so a plain `claude` routes: a subscription
session sends the router secret in `X-Claude-Code-Router` (via `ANTHROPIC_CUSTOM_HEADERS`, no `apiKeyHelper`) and
keeps its OAuth login in `Authorization`, which the router forwards to `passthrough` providers only. A
`passthrough` slot under an API-key session provider is refused — that session carries no login to forward.

## Agents

```bash
claude-code agents                                   # runners and whether each is available
claude-code ask --agent codex "Review the diff on this branch for correctness bugs."
claude-code ask --log                                 # recent dispatches: id, status, agent, duration, prompt preview
claude-code ask --retry <id> [--timeout 20m]          # replay one exactly; flags typed here override the record
claude-code ask --agent gemini --format json "Map every caller of providers.Select."
claude-code run opencode --provider openrouter --model qwen/qwen3-coder
claude-code run codex --print-env                    # show the env/argv wiring, launch nothing
claude-code codex ask "Review the diff on this branch for correctness bugs."   # top-level: codex|copilot|opencode
claude-code codex --print-env                         # bare verb = `run codex`; `ask` = one-shot task
claude-code science serve                            # Claude for Life Sciences on its own login, harness env stripped
claude-code usage --days 7 [--workflows]             # subagent tokens (one count per API response) + peak ITPM
claude-code gemini bridge --dirs internal --index "map every caller of providers.Select"  # one 1M-context call
claude-code agents sync --dry-run                    # agents/ → dist/<vendor>/agents + vendor homes; skills/ → ~/.agents/skills
claude-code agents docs                              # AGENTS.md + GEMINI.md from CLAUDE.md + the roster
```

Exec runners (codex, gemini, opencode) and the interactive `run` launch share one wiring function, so a
gateway route written once serves both. Without `--tools`, claude and opencode get no tools, codex a read-only
sandbox, and gemini its read-only plan mode. gemini reads the key only when `~/.gemini/settings.json` selects
API-key auth (or none); a Google login there overrides every env var, so the runner refuses it. An interactive
`run opencode` also imports your Claude permissions (`settings.json` allow/deny/ask → opencode's `permission`
block) and MCP servers (`~/.claude.json`, plus project `.mcp.json` entries Claude Code has approved for that
directory — unapproved ones are named in `HARNESS_OPENCODE_MCP_SKIPPED`) and lists every tier model the provider
serves; `--print-env` redacts MCP env, headers, key/token argv and URL queries.

`claude-code gemini` is the gemini agent verb: `gemini ask "<task>"` = `ask --agent gemini --provider gemini`
(drives the gemini CLI); `gemini bridge [--dirs|--files|--index|--diff|--rubric <file>] "<task>"` inlines
workspace files into one prompt and makes a single first-party call on the `gemini` provider (`GEMINI_API_KEY`,
default `gemini-3.8-flash`, `--model opus` for the reasoning tier) — the 1M-context sweep the `gemini` subagent
and `/gemini:gemini` / `/gemini:second-opinion` wrap. Inside a git tree only tracked/untracked-but-not-ignored files
are eligible, and `.env*`, `secrets.env`, `env.d/`, `state/`, `*.pem`/`*.key` are always skipped; the inlined path
list is printed to stderr before the call. Anything else launches the gemini CLI (same as `run gemini`).
`agents sync` projects `agents/` into every vendor's native subagent format — `~/.codex/agents/<name>.toml`,
`~/.copilot/agents/<name>.md`, `~/.gemini/agents/<name>.md`, `~/.config/opencode/agents/<name>.md` — via a
git-tracked rendering under `dist/<vendor>/agents/` (the home copy is a byte-identical install of it), and links the
hand-authored `skills/` into `~/.agents/skills`, the one skills dir all four CLIs read natively (so no vendor sees a
skill twice). Every generated file is banner-marked; hand-authored files, symlinks into other trees and real dirs are
kept and reported; `--check` gates drift in both `dist/` and the homes; `--vendor <v>` narrows a run. `install` runs it
after settings. `agents docs` renders `AGENTS.md` + `GEMINI.md` (generated, gitignored like `settings.json`). Inside a session, the subagents in `agents/` (`code-workers`,
`test-driven-dev`, `test-repair`, `gemini`) and the Workflow scripts in `workflows/` handle in-process fan-out; see
`rules/workflow/master-workflow.md`.

## Hooks

Declared in `internal/hookspec/hookspec.go`, implemented in `internal/hooks/`, rendered into
`settings.json` as `$HOME/.claude/bin/claude-code hook <name>`:

| Hook | Event | What it does |
| --- | --- | --- |
| `safety` | PreToolUse (Bash) | Blocks destructive shapes: `rm -rf /`, force-push to main, `curl \| sh`, `sudo`, `DROP TABLE`, `git stash/checkout/reset/restore/clean` aimed at `~/.claude`, … |
| `rtk-wrap` | PreToolUse (Bash) | Routes noisy commands (`git`, `npm`, `cargo`, `kubectl`, `pytest`, `docker`) through the [rtk](https://github.com/rtk-ai/rtk) output compressor when it is on PATH; machine-readable flags (`--porcelain`, `--json`, …) and value subcommands (`rev-parse`, …) bypass it. `HARNESS_RTK_BIN` overrides the binary |
| `docs-placement` | PreToolUse (Write\|Edit) | Blocks a new `.md` at a repo root or under a git-ignored `docs/`, naming the canonical home (`~/.claude/docs/plans\|reports\|handoffs`); root anchors and tracked files pass. `HARNESS_DOCS_PLACEMENT=0` disables |
| `agent-effort` | PreToolUse (Agent) | Fills an UNSET subagent model from a local prompt classifier (low→haiku, else sonnet); an explicit `model:` on the dispatch or in the agent's frontmatter always wins |
| `context-checkpoint` | Stop | Once per session at ≥85% context, blocks Stop so the model can hand off before compaction |
| `stop-format` | Stop | Formats files edited this turn: the repo's own `make format\|fmt` target when it honors a per-file variable (`TARGET`/`FILE`/…), else the pyproject-declared python chain, else gofmt/ruff/prettier/shfmt when installed |
| `session-start` | SessionStart | Injects the local memory bank (char-capped); an indexed global bank is a one-line pointer |
| `memory-recall` | UserPromptSubmit | Injects up to 3 bank/native-memory blocks matching ≥2 prompt terms, skipping any already in this session's context (≥4 terms, ≤1,500 chars; else silent) |
| `session-harvest` / `session-harvest-end` | Stop / SessionEnd | After ≥3 edits, spawns a detached `claude-code memory update` that runs `/memory:end`; the first dispatch also claims one `memory watch` per session, which harvests the final state once the transcript has been quiet 10 min (a crash or kill fires no SessionEnd) |
| `access-trace` | PostToolUse (Read) | Logs a model read of a bank file (or its index) to that bank's `access.log`, the signal behind `memory doctor`'s hot/cold nominations |

To add one: implement it in `internal/hooks/`, add a `Spec` to `hookspec.Registry`, `make install`.

## Memory bank

Six files per git repo under `docs/memory/bank/<repo-slug>/` (untracked): `projectContext`, `activeContext`,
`progress`, `decisionLog`, `conventions`, `sessionHistory`; `docs/memory/bank/` itself is the global bank, used
by any non-repo cwd. A pre-`bank/` tree moves in place on the next session start or `memory` verb.

What a session pays for: SessionStart injects `activeContext` (minus its `## Parked` section) + `progress` and the bank's generated
`index.md` (one routing line per durable entry, ≤8,500 chars total); everything else is found per prompt by the
`memory-recall` hook (BM25 over local, global, archive and native topics, ≤3 blocks, skipping what is already in
context) and by `claude-code memory search`. Every settle (`memory index`, the background harvest) keeps the bank
small: `sessionHistory` rotates past 30 entries to `archive/`, local `decisionLog`/`conventions` overflow past
their line caps is promoted to the global bank, global overflow is archived, and `index.md` is regenerated.

```bash
claude-code memory init | path | index              # scaffold, locate, settle + reindex
claude-code memory search <terms> [--k 8] [--format json]  # BM25 over every tier
claude-code memory doctor [--stale-days 90]         # cap offenders, staleness, credential-shaped lines, hot/cold topics
claude-code memory stats | timeline [-n 20] | promote
```

**Native auto-memory** (Claude Code's own `MEMORY.md` + topic files under `projects/<slug>/memory/`) is injected
whole every session, so `claude-code memory archive` governs its size: the audit reports injected bytes, orphans
and dangling pointers; `--split 20 --apply` keeps a 20-line core in `MEMORY.md` and moves older pointer lines to
`SEARCHABLE.md` (indexed by recall, never injected; a `<!-- keep -->` marker pins a line); `--dupes` clusters
near-duplicate topics; `archive <topic> --apply` moves a cold topic out (pointer lines recorded verbatim) and
`--restore` brings it back. `access.log` beside each bank records every recall, which is what makes a topic
"hot" (a pointer is suggested) or "cold" (an archive candidate) in `memory doctor`.
`/memory:init` enriches, `/memory:end` saves manually.

## Housekeeping

- `claude-code clean [--what <targets>] [--older-than 30d] [--list]` — sweeps accumulated state (caches,
  run dirs, backups); dry-run by default, `--apply` deletes. Credentials and settings are never a target.
- `claude-code history snapshot|log|diff` — shadow git history of config + memory, kept at
  `state/harness/history`; `env.d/secrets.env` is never snapshotted.
- `claude-code theme [--preview]` — pick the statusline color palette.
- `claude-code audit [--days 7] [--format json]` — the typed action log: provider switches, model pins, secret sets (name only), safety-hook blocks (reason, never the command), router failovers. `doctor` prints the 7-day count.
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
docs/plans/          implementation plans
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
