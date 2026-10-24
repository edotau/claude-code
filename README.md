# claude-code

A minimal harness for [Claude Code](https://docs.anthropic.com/en/docs/claude-code). This repo **is** `~/.claude`:
one Go binary, `claude-code`, plus the agents, commands, rules, skills and Workflow scripts every session loads.

What it adds to a stock install:

- **Provider registry** — Anthropic, Claude/ChatGPT/Copilot subscriptions, OpenAI, Gemini, Nvidia, Ollama or any
  gateway; tier slots (opus/sonnet/haiku/fable) map to model ids per provider.
- **Loopback router** — OpenAI-dialect translation, per-tier provider mixing, failover on 429/5xx.
- **Agent runners** — one-shot tasks on Claude, Codex, Gemini, OpenCode or Copilot (`ask`), interactive CLIs (`run`).
- **Hooks, memory, status line** — a Bash safety gate, a per-repo memory bank, format-on-stop, context checkpoint.

Every verb takes `--help`; `claude-code describe` maps the whole harness.

## Install

```bash
git clone https://github.com/edotau/claude-code ~/.claude
cd ~/.claude
make install                  # build, link vendor CLIs into bin/, render settings.json (DEV_TOOLS=1 adds linters)
claude-code doctor            # check binaries, providers, credentials, router
```

- `make bootstrap` — fresh box: Go toolchain (no sudo), install, bash completion, then `claude-code configure`
  (`CHECK=1` runs `configure --check` instead; `TAILSCALE=1` adds tailscale).
- `claude-code install --dry-run` previews; foreign hooks in `settings.json` are kept, with a timestamped backup.
- A missing vendor CLI is reported, never downloaded; `install --tools=<name>` installs it.
- `bin/<vendor>` is the real, unrouted CLI. Routed launches go through `claude-code <vendor>`.

![claude-code help menu](docs/images/cli-menu.svg)

```bash
claude-code opus        # Claude Code with opus as the main model (also sonnet | haiku | fable)
claude-code codex       # a vendor CLI through the registry (also gemini | opencode | copilot)
claude-code ollama      # Claude Code on that registered provider
claude-code --resume    # a leading flag passes to claude, on the session provider
```

## Setup

```bash
claude-code configure                 # interactive menu: providers, logins/keys, models, agents; Submit seals keys
claude-code configure models          # one section (provider|models|agents|router|github|remote|browser|claude|doctor)
claude-code configure claude          # per-machine settings overrides; team defaults stay in the template
claude-code configure --check         # report only; exit 1 if anything is unset
claude-code generate [--out f|--check|--dry-run]   # render settings + vendor configs
claude-code generate --revert [--agents a,b]       # restore vendor configs from backup
claude-code vscode [--check|--dry-run]             # merge VS Code settings + keybindings
```

- `configure` reuses what already exists (env, vault, vendor logins) and prompts only for what is missing.
- **Claude subscription** keeps Remote Control; **API key** pays per token. For headless runs on a Max plan,
  `configure` mints `claude setup-token` into the vault as `CLAUDE_CODE_OAUTH_TOKEN`.
- Vendor configs are merged, never replaced: `~/.codex/config.toml`, `~/.config/opencode/opencode.json`,
  `~/.gemini/antigravity-cli/settings.json`, Codex/Copilot hooks. Keys are referenced by env var, exported only by a
  routed launch.
- On a terminal, `generate` prompts before overwriting hand-edited lines it would otherwise lose; a
  non-interactive run (launch, `configure`, `make install`) writes and warns instead. Each overwrite keeps 5
  timestamped backups; `generate --revert` steps a vendor file back one version at a time.
- From a worktree, render only that checkout: `CLAUDE_CONFIG_DIR="$PWD" claude-code generate --out "$PWD/settings.json"`
  (vendor homes are skipped unless `--vendors`).

### Setup modes: adopt, nuke, workspace

```bash
git clone https://github.com/edotau/claude-code ~/.claude.stage      # ~/.claude already exists: stage the clone,
make -C ~/.claude.stage adopt                                         # then adopt it (backs up collisions) + bootstrap
claude-code install --setup nuke [--apply] [--keep-memory]            # archive untracked content, then remove
claude-code install --setup workspace init|link|status                # scope a project's own .claude/
```

## Providers

Built-ins live in `internal/providers/defaults.json`. `claude-code vault edit` overlays them **by name** (an entry
replaces the built-in; `null` removes it).

```jsonc
"providers": {
  "default": "gateway",
  "fallback": ["gateway", "anthropic"],         // 429/5xx ladder (implies the router)
  "providers": {
    "gateway": {
      "kind": "anthropic",                       // dialect base_url speaks
      "base_url": "https://llm.example.com",
      "routes": {"openai": "/v1"},               // extra dialects on the same gateway
      "auth": {"type": "bearer", "env": "GATEWAY_API_KEY"},
      "models": {"opus": "claude-opus-5", "sonnet": "claude-sonnet-5", "haiku": "gpt-5-nano"},
      "one_m": true                              // adds [1m] to Claude ids
    },
    "ollama": null
  }
}
```

| Field | Meaning |
| --- | --- |
| `kind` | `anthropic` · `openai` · `responses` · `gemini` |
| `auth.type` | `x-api-key` · `bearer` · `none` · `passthrough` (subscription OAuth; `auth.login` names the vendor CLI) |
| `models` | slot → model id |
| `headers` | sent on every upstream request |

```bash
claude-code profile [name]                          # providers, credential state, fallback ladder
claude-code switch gateway                          # pin the session provider
claude-code models --pin haiku=openai:gpt-5-nano    # pin a slot; provider:model crosses providers
claude-code models --unpin | --live --provider p    # clear pins | list what upstream serves
claude-code --provider gateway --model claude-sonnet-5   # this launch only
```

Precedence: `--provider` → `HARNESS_PROVIDER` → registry `default`. Slot: `HARNESS_<SLOT>_MODEL` → `HARNESS_MODEL` →
the provider's `models`. Process env beats vault pins.

### Vault: `claude-code vault show|edit`

`state/vault.db` holds pins, custom launch env, provider/vendor overlays and `key_command` as one JSON document, plus
sealed secrets. `vault edit` opens it in `$EDITOR` and saves atomically; secrets never appear in it.

```jsonc
{
  "pins": {"HARNESS_PROVIDER": "anthropic", "HARNESS_HAIKU_MODEL": "openai:gpt-5-nano"},
  "env": {"MY_PROJECT_ROOT": "/home/me/work", "SOME_API_TOKEN": "@secret"},   // @secret = vault secret of that name
  "providers": {"default": "gateway"},
  "vendors": {"vendors": {"opencode": {"mcp": {"allow": ["github"]}}}},
  "vault": {"key_command": ""}
}
```

## Credentials

First non-empty source wins:

1. **Process env** — `auth.env`
2. **Vault** — `claude-code secrets set <KEY>` (AES-256-GCM)
3. **`auth.file`**
4. **`auth.command`** — prints the credential; cached `auth.ttl` seconds (default 300), re-run on 401

```json
"auth": {"type": "bearer", "command": "op read op://Private/gateway/credential", "ttl": 3600}
```

`claude-code token --provider <p>` is the `apiKeyHelper` Claude Code calls, so no key lands in `settings.json`.

```bash
claude-code secrets set [KEY]          # bare on a terminal = guided dialog
claude-code secrets get KEY [--export]
claude-code secrets import keys.env    # then delete the plaintext file
claude-code secrets export --out - | ssh box claude-code secrets import - --yes
claude-code secrets status|scan|git|doctor|all   # sources and leak audits; never print values
```

### The vault key

32 random bytes, resolved in order: `HARNESS_VAULT_KEY` → OS keyring (service `claude-code`) → `key_command` (a JSON
argv run without a shell). No key file is written. Headless boxes set `HARNESS_VAULT_KEY` or `key_command`.
`HARNESS_VAULT_KEY` is scrubbed from vendor CLI children. `vault rotate-key` re-seals and keeps old keys.

## Routing

`claude` launches **direct** when the provider speaks `anthropic`, every slot stays on it, and no `fallback` is set.
Otherwise it goes through the **loopback router** (`127.0.0.1:18765`, started on demand), which:

1. requires a per-install client secret and a loopback `Host`;
2. resolves `model` (`id` or `provider:model`) and strips `[1m]`;
3. passes `anthropic` routes through; translates Messages ⇄ chat-completions for `openai` routes (streaming, tools);
4. refreshes the credential and replays once on 401/403;
5. fails over along `fallback` on 429/5xx/connect error before the first byte;
6. on a hard-quota 429 demotes opus → sonnet → haiku and skips the dead model for 30 minutes.

Provider hostnames resolving into the local network are refused (a literal IP or `localhost` in `base_url` is allowed).
The router disables Remote Control, so a subscription session stays direct under a `fallback` ladder; a
cross-provider slot or `--router` still routes it.

```bash
claude-code router start|status|stop
claude-code router serve --port 18765   # foreground
claude-code --router                    # force the router for one launch
```

## Tailscale

```bash
claude-code tailscale status|machines|connect|key [--save]|exit-node
```

`status`/`machines`/`connect` use the installed CLI. `key` and `exit-node --approve` need
`TAILSCALE_OAUTH_CLIENT_ID`/`_SECRET` (preferred) or `TAILSCALE_API_KEY` in the vault. `make tailscale-cli` installs it.

`claude-code vscode serve [--port N] [--stop] [--status] [--no-tailscale] [--funnel]` runs
`code serve-web` as a detached daemon and maps it onto the tailnet (or the public internet with
`--funnel`), printing a token-gated URL and a terminal QR code for your phone.

## Worktrees and agent views

```bash
claude-code worktree list|create <name>|clean <name>|clean-all   # ~/worktrees/<repo>/*; never forces a removal
claude-code agents tree [--watch]          # sessions → subagents → workflows, with vendor legs and fallbacks
claude-code web view <agentId> [--follow]  # one agent's snapshot or live events
claude-code web [--addr ip:port]           # dashboard over loopback or the tailnet
claude-code dispatch --host <h> --agent <a> "<task>"   # detached agent TUI on another box (tmux)
claude-code web browsers | setup --browser edge         # browser control paths; Claude in Chrome install
claude-code web search --query q           # Tavily one-shot (bare = stdio MCP server)
```

## Agents

```bash
claude-code agents                                     # runners and availability
claude-code ask --agent codex "Review this branch."    # one-shot; --format json, --tools, --effort
claude-code ask --log | --retry <id>                   # recent dispatches; replay one
claude-code run opencode --provider ollama --model qwen3-coder   # interactive; --print-env shows wiring
claude-code gemini bridge --dirs internal "<task>"     # one 1M-context sweep via agy
claude-code agents sync [--dry-run|--check]            # project agents/ + CLAUDE.md into every vendor
claude-code usage --days 7                             # subagent token usage
```

- Without `--tools`, runners are read-only. Gemini runs only on `agy` and its Google sign-in.
- `gemini bridge` inlines tracked files only and always skips `.env*`, `state/`, keys.
- `agents sync` renders under `state/projections/<vendor>/`, installs into `~/.codex`, `~/.copilot`, `~/.gemini`,
  `~/.config/opencode`, and links `skills/` into `~/.agents/skills`. No AGENTS.md lives at this root (Claude Code would
  load it as a second CLAUDE.md).
- In-session fan-out: `agents/` subagents and `workflows/` scripts — see `rules/workflow/master-workflow.md`.

## Hooks

Declared in `internal/hookspec/hookspec.go`, implemented in `internal/hooks/`. To add one: implement, register a
`Spec`, `make install`.

| Hook | Event | Does |
| --- | --- | --- |
| `bash-guard` | PreToolUse Bash | Blocks destructive commands (`rm -rf /`, force-push main, `curl \| sh`, git resets in `~/.claude`); routes noisy output through rtk |
| `docs-placement` | PreToolUse Write/Edit | Blocks new `.md` at a repo root or ignored `docs/`; enforces the plan template |
| `agent-effort` | PreToolUse Agent | Fills an unset subagent model (low → haiku, else sonnet); logs the decision; appends up to 3 memory recall pointers to the spawn prompt |
| `agent-routed` | SubagentStart | Records the subagent's actual launch model against `agent-effort`'s decision log |
| `workflow-lint` | PreToolUse Workflow | Pins unpinned inline agents to sonnet |
| `agent-inflight*` | Pre/PostToolUse Agent | Caps concurrent dispatches at 8 |
| `subagent-stop` | SubagentStop | Retires the stopping subagent's inflight slot |
| `plan-mirror` / `save-plan` | PostToolUse | Dated plan copies under `plans/`; files the approved plan |
| `context-checkpoint` | Stop | At ≥85% context, pauses once for a handoff |
| `stop-format` | Stop | Formats files edited this turn |
| `session-start` | SessionStart | Injects the memory catalog and this repo's active context |
| `memory-recall` | UserPromptSubmit | Injects up to 3 matching memory blocks |
| `wsl-path-normalize` | UserPromptSubmit | Maps pasted Windows paths to `/mnt/…` |
| `session-harvest[-end]` / `pre-compact` | Stop / SessionEnd / PreCompact | Saves the session into the memory bank |
| `access-trace` | PostToolUse Read | Logs bank reads for `memory review` |
| `worktree-create` / `-remove` | Worktree* | Under `~/worktrees/<repo>/`; refuses dirty removals |

## Memory bank

One store, `~/.claude/memory/`: a local-only git repo (every settle commits, never pushed).

```text
memory/
  index.md       generated catalog — never hand-edit
  home/          cross-repo bank
  claude-code/   the harness's own bank
  <repo>/        per repo, keyed by origin remote basename; .origin records identity
```

Each bank dir holds Claude Code's native memory (`MEMORY.md` + topic files) and six typed files: `projectContext`,
`activeContext`, `progress`, `decisionLog`, `conventions`, `sessionHistory`. `MEMORY.md`'s
`<!-- bank:begin -->…<!-- bank:end -->` region is generated; lines outside it are never touched.
`~/.claude/projects/<slug>/memory` is a symlink into the store.

A session gets the catalog plus this repo's active context and progress; the rest loads on demand. Settles rotate
`sessionHistory` past 30 entries into `archive/`. `memory dream` consolidates daily in the background.

```bash
claude-code memory recall [--repo R|--all] <terms>    # BM25 search; public verb
claude-code memory init|review|promote|remember|update|dream   # public verbs
claude-code memory path|index|show <id>|digest|timeline|fmt|watch   # private plumbing
claude-code memory archive [--split N --apply|--dupes]  # keep MEMORY.md small
claude-code memory migrate|alias                     # one-time cutover; fold a second checkout
```

`claude-code memory init` enriches a bank; `claude-code memory update` saves manually.

## Housekeeping

```bash
claude-code clean [--list|--dry-run] [--target projections|sessions]   # sweep caches, run dirs, backups
claude-code history snapshot|log|diff     # shadow history of config (never secrets)
claude-code doctor --audit [--days 7]     # provider switches, pins, secret sets, guard blocks, failovers
claude-code mcp doctor                    # MCP health, scope drift, inline secrets
claude-code theme [--preview] | apply     # statusline palette; whole look across tools
```

## Status line

`claude-code statusline [--style gradient|powerline|capsule|minimal|dashboard]` shows model, provider, router, context
%, tokens, duration and git state. Config: `~/.claude/state/statusline/config.json`.

## Layout

```text
cmd/claude-code/     main
internal/            cli (verb table) · providers · router · translate · launch · vendors · agent · hooks · memory …
agents/ commands/ rules/ skills/ workflows/   loaded by Claude Code
state/vault.db       pins, env, overlays, sealed secrets (untracked)
memory/              the memory store (its own local git repo)
.memory-cache/       harness backups (*.bak-*), shadow config history, vendor caches (untracked)
plans/               implementation plans (untracked)
docs/templates/      index of every embedded (`//go:embed`) template, with rendered examples
```

## Command reference

Rows `CLAUDE.md` points here for.

| To… | Run |
| --- | --- |
| Set up a box end to end | `claude-code configure [section] [--agent a] [--check]` · `make bootstrap` |
| Adopt, wipe or scope a `.claude/` | `claude-code install --setup adopt\|nuke\|workspace` |
| Sync VS Code settings | `claude-code vscode [--check\|--dry-run]` |
| Bash tab completion | `claude-code completion --install` |
| Add a gateway provider | `claude-code vault edit` → `providers.providers` |
| Edit pins, env, overlays | `claude-code vault show\|edit\|migrate\|finalize\|rollback\|rotate-key` |
| OpenCode reasoning effort | TUI `ctrl+t` · `opencode run --variant high` · `claude-code ask --effort high` |
| Project agents into vendors | `claude-code agents sync [--vendor v] [--dry-run\|--check]` |
| Run an agent on another tailnet box | `claude-code dispatch --host <h> --agent <a> "<task>"` · `--list --status` |
| Watch agents from a phone | `claude-code web [--addr ip:port]` |
| Subagent status and legs | `claude-code agents tree [--watch]` · `claude-code web view <id>` |
| Browsers Claude in Chrome can drive | `claude-code web browsers` · `web setup --browser <b>` |
| Audit what acted | `claude-code doctor --audit` |
| Config history | `claude-code history snapshot\|log\|diff` · memory: `git -C ~/.claude/memory log` |
| Theme | `claude-code theme [--preview] [<palette>]` · `theme apply` |
| Keep MEMORY.md small | `claude-code memory archive [--split 20 --apply]` |
| Lint a workflow script | `claude-code workflow lint` · `workflow skeleton <pattern>` |
| Tailnet | `claude-code tailscale status\|machines\|connect\|key\|exit-node` |
| VS Code on your phone | `claude-code vscode serve [--stop\|--status] [--funnel]` |

## Development

```bash
go test ./internal/<pkg>   # while developing — only what you touched
make lint                  # fmt, vet, shell, yaml, markdown (CHECK=1 reports only)
make lint-deep             # golangci-lint, uncapped
make test                  # full gate, once before pushing (= make audit = CI)
```

- `git push` runs the same gate via `.githooks/pre-push`; `--no-verify` bypasses it.
- `settings.json` is generated: edit `internal/settings/template/`, then `make install`. Template keys reset
  on every install; permission lists are unioned, so a removed rule must also be deleted from `settings.json` by hand.
