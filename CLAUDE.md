# CLAUDE.md

Injected into **every** session's system prompt — an index of rules and how-tos, not an exposition.
State the rule, point to the depth: `README.md` (infrastructure), `rules/` (policy), skills (how-to).

## What this repo is

This git repo **is** `~/.claude` (github.com/edotau/claude-code): the `claude-code` Go binary
(provider registry, loopback model proxy, Anthropic⇄OpenAI translation, agent launchers, hooks,
status line) plus the agents, commands, rules, skills and workflows every Claude Code session loads.
Files under `agents/`, `commands/`, `rules/`, `skills/` and this file go live the moment they are saved.

## Hard rules (always on)

1. **`settings.json` is generated — never hand-edit it.** Edit `internal/settings/template/`
   (hooks: `internal/hookspec/hookspec.go`), then `make install`. Only `settings.local.json` is hand-edited.
2. **Secrets live only in the encrypted vault (`state/vault.db`; `claude-code vault set`) or a provider's
   `auth.command`** — never in a plain file, `settings.json`, a commit, or a log line. The vault's master key
   comes from the OS keyring, `HARNESS_VAULT_KEY`, or `key_command`.
3. **Commit messages carry NO AI-attribution footers** (`Co-Authored-By: Claude`,
   `Generated with Claude Code`, …) — this overrides any default that says to add them.
4. **Never `git stash` / `checkout` / `reset` / `restore` in the live checkout (`~/.claude`).** It is
   one checkout shared by parallel subagents and every live session. A linked worktree
   (`~/worktrees/<repo>/<slug>`, shared with Orca, via `EnterWorktree` or `claude-code worktree create`) is a
   separate checkout: branch there freely, merge back with `claude-code worktree merge <slug>`
   (verified, fast-forward-only: `skills/using-git-worktrees/references/merge-and-clean.md`), never
   `checkout`/`reset` the live tree to it.
   `make install` and `claude-code` verbs still write to `~/.claude` from a worktree unless `CLAUDE_CONFIG_DIR`
   points at it.

## Behavioral guidelines

1. **Think before coding.** State assumptions; surface multiple interpretations instead of picking
   silently; name a simpler approach if one exists.
2. **Simplicity first.** Minimum code that solves the problem → `rules/standards/communication.md`.
3. **Surgical changes.** Touch only what the request requires; match existing style; don't refactor
   adjacent code. Remove only orphans your change created; mention pre-existing dead code.
4. **Goal-driven execution.** Turn tasks into verifiable goals ("fix the bug" → "reproduce it, fix,
   re-run"). Loop until verified. → 1–4 in depth: `skills/evaluate-code/references/principles.md`.
5. **Terse comments.** 1 line max, ≤120 chars, per section → `rules/standards/communication.md`.
6. **Live check first; scoped tests while developing; the full suite once, with the user's approval.** Tests verify
   logic: one spanning test per feature, a regression test per bug, no coverage waves → `rules/standards/quality.md`.
7. **Propose look/behavior changes before applying them.** Name an unrequested side effect with
   numbers, prefer the narrowly scoped mechanism over a global knob, and let the user pick.
8. **Parallel by default; keep the main thread free.** Plan and brief, then dispatch `code-worker` ‖ `copilot`
   in one message in the background; inline only for a one-line edit; never re-steer a running agent →
   `rules/standards/quality.md`. Many leaves: one lane orchestrator per lane →
   `skills/subagent-workflows/references/lane-orchestrator.md`.
9. **Tag notable moments** in replies, one per line: `DECISION:` (what, rejected alt, why) · `BUG:` · `NEXT:` ·
   `NOTE:` — the harvest files them.

## How-to (task → command → depth)

| To… | Run | Then / depth |
| --- | --- | --- |
| Build and install (`claude-code` + agent links in `~/.claude/bin`, render `settings.json`) | `make install` (`claude-code install --dry-run` previews) | `README.md` → Install |
| Render settings + agent configs | `claude-code configure --reset [--out f] [--check] [--dry-run] [--agents]` | hard rule 4; `README.md` → Setup |
| Check binaries, providers, credentials, proxy | `claude-code doctor` | every diagnostic verb takes `--format text\|json` (doctor, configure provider --list, usage, history log, network mcp, memory search) |
| See providers and credential state | `claude-code configure provider --list [<name>]` | `README.md` → Providers |
| Switch the session profile | `claude-code switch <profile> [--dry-run]` (relaunch); one launch: `claude-code --profile <p>` | pin lands in the vault (`claude-code vault list`) |
| Pin models (`--dry-run` previews any pin change) | `claude-code models --pin opus=<id>` · `--pin all=<id>` · `--unpin` | pins are vault-only (`CLAUDE_<TIER>_MODEL`); `--model` beats them |
| List what a provider serves | `claude-code models --live [--profile <p>]` | — |
| Set a credential | `claude-code vault set [NAME]` (sealed in the vault; bare on a terminal = guided dialog), or `auth.command`; also `vault list\|get\|delete\|import\|export`, sources in `configure provider --list` | `README.md` → Credentials |
| Run the model proxy | `claude-code gateway proxy --start\|--stop\|--status` (`--serve --port N` in the foreground) | `README.md` → Routing |
| Ask another agent (one shot) | `claude-code ask --agent <agent> "<task>"` (`--format json` = envelope); `ask --log` lists recent dispatches, `ask --retry <id>` replays one; `claude-code agents` lists the CLI agents | `skills/subagent-workflows/references/cost-routing.md` |
| Search the public web when native search is unavailable | `claude-code network websearch --query "<query>"` | `skills/web-search/SKILL.md` |
| Sweep many files in one Gemini call (1M context) | `claude-code gemini bridge --dirs <d> [--index] [--diff] [--rubric <file>] "<task>"`; `gemini ask "<task>"` drives agy (the Antigravity CLI) | `commands/gemini.md`, `agents/gemini.md` |
| Launch codex/gemini/opencode/copilot, or drive one by verb | `claude-code codex [--profile <p> --model <m>] [args]` (`--` forces passthrough); `claude-code <v> ask\|resume\|review\|transfer\|status\|result\|cancel\|setup\|models`; `--print-env` shows the wiring | `README.md` → Agents; `agents/<v>.md` |
| Project agents + this file into every agent CLI, link skills into `~/.agents/skills` | `claude-code agents sync [--agent a] [--dry-run\|--check]` (also run by `install`) | `rules/harness/docs-placement.md` → What stays out of the three homes |
| List, create, clean worktrees under `~/worktrees/<repo>/` | `claude-code worktree list [--format json]` · `create <name>` · `merge <name>` · `clean <name>` · `clean-all [--yes]` | `skills/using-git-worktrees/references/merge-and-clean.md` |
| Add or retime a hook | implement in `internal/hooks/`, register in `internal/hookspec/hookspec.go`, `make install` | `README.md` → Hooks |
| Change permissions / env / statusLine | edit `internal/settings/template/` → `claude-code configure --reset --dry-run` (prints) → `make install` | hard rule 1 |
| Check MCP server health, scope drift, inline secrets | `claude-code network mcp [--format json]` | `rules/harness/settings-management.md` |
| Map the whole harness for an agent (verbs, providers, agents, hooks) | `claude-code describe [--format json\|text]` | credentials never appear, only whether one resolves |
| Sweep accumulated state (caches, run dirs, backups) | `claude-code clean [--list]` (applies by default; `--dry-run` previews; opt-in `--target projections\|sessions`) | credentials/settings never swept |
| Recall or save session memory | `claude-code memory recall <terms>` · `memory update` (auto via the Stop/SessionEnd harvest) · `memory init\|review\|promote\|remember\|dream` | `README.md` → Memory bank |
| Test while developing | `go test ./internal/<pkg>` (one test: `-run '<Name>'`); `make lint` | full `make test` only at wrap-up, user-approved; `git push` runs the same gate via `.githooks/pre-push` (`--no-verify` bypasses) |
| Build a MEDIUM+ change (2+ files or needs tests) in ANY repo | `/code-workers` leaves on disjoint packages ‖ `copilot` test-repair agents, same message; the `subagent-workflows` skill's `orchestrate.md` for explore → plan → dispatch | `rules/workflow/master-workflow.md` → Phase 6 |
| Run a saved Workflow (only when the user asks for one) | `Workflow code-engineer` · `code-analyzer` · `green-keeper-loop` · `code-agent` | `skills/subagent-workflows/references/saved-workflows.md` |
| Review a diff; catalog tech debt | `/code-review` (quality + security lenses) | `skills/evaluate-code` (security → Security lens; debt: `references/techdebt.md`) |
| Auto-fix lint, agent fixes the rest | `make lint-fix [WHAT=go\|md\|all] [AGENT=copilot] [MAX=3]` · in-session `/lint-fix` | `commands/lint-fix.md` |
| Fix failing tests | `/copilot test`, or `/testing-guidelines` (test-and-fix loop) | `skills/testing-guidelines/SKILL.md` → Test-and-fix loop |
| Capture a session's corrections into rules | `claude-code memory remember <fact>` then `memory promote <id> --to rules` (routing table: `claude-code memory promote` procedure) | `rules/workflow/memory-consolidation.md` |
| Audit / recall / remember / promote / save / consolidate memory | `claude-code memory review\|recall\|remember\|promote\|update\|dream` | `agents/opencode.md` |
| Second-opinion adversarial review on another model | `/codex` · `claude-code codex review [--adversarial] [--base <b>]`; vet a brief: `codex ask` + `task-review.md` | `agents/codex.md` |
| Route a task to the cheapest capable agent | classify `sweep\|review\|mechanical\|standard\|reasoning`, then `claude-code gemini bridge` / `ask --agent <agent>` | `skills/subagent-workflows/references/cost-routing.md` |
| Scan the config surface / MCP for leaked credentials | `claude-code doctor --scan\|--git\|--secrets` · `claude-code network mcp` | `rules/standards/security.md` |
| Everything else (configure, VS Code, network, theme, history, …) | `claude-code describe` | `README.md` → Command reference |

## Where things live

- `cmd/claude-code/` + `internal/` — the binary, stdlib-first; external SDKs boxed one package each.
  `internal/cli` is the verb table only; each verb's engine lives in its own package.
- `internal/providers/defaults.json` — built-in providers; the vault's provider overlay replaces them by name.
- `state/vault.db` — the vault: pins, custom env, provider/agent overlays (`claude-code vault list|edit`) and
  sealed secrets (`vault set|get`); untracked.
- `agents/<name>.md` — subagents, flat (the layout VS Code Copilot Chat also reads); `name:` **must equal the file
  stem**.
- `commands/<command>.md` — slash commands, flat. `workflows/*.js` — Workflow tool scripts.
- `rules/` — `standards/` (always-on) · `workflow/` · `software/<lang>/`; scoping contract → `rules/README.md`.
- `skills/<name>/SKILL.md` — on-demand references; untracked skill dirs are the user's own (symlinks into
  `~/.agents`, which Gemini reads natively), leave them.
- `skills/synced/` — claude.ai account skills Claude Code re-downloads each sync round; untracked, edit them on
  claude.ai.
- `plans/` — implementation plans (Claude Code's native `plansDirectory`); untracked, gitignored
  wholesale — this repo's own at the top, other repos' under `plans/<repo-slug>/`.
- `memory/` — the one memory store: a local-only git repo. `memory/index.md` is the generated catalog
  (router, never hand-edit); `memory/<repo>/` holds both native auto-memory (MEMORY.md + topics) and
  the six typed bank files per repo (harness itself = `memory/claude-code/`); home bank in `memory/home/`.
  `projects/<slug>/memory` is a symlink into it. Engine `internal/memory`, hooks `session-start` /
  `memory-recall` / `session-harvest[-end]`.
