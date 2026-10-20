# CLAUDE.md

Injected into **every** session's system prompt — an index of rules and how-tos, not an exposition.
State the rule, point to the depth: `README.md` (infrastructure), `rules/` (policy), skills (how-to).

## What this repo is

This git repo **is** `~/.claude` (github.com/edotau/claude-code): the `claude-code` Go binary
(provider registry, loopback model router, Anthropic⇄OpenAI translation, agent runners, hooks,
status line) plus the agents, commands, rules, skills and workflows every Claude Code session loads.
Files under `agents/`, `commands/`, `rules/`, `skills/` and this file go live the moment they are saved.

## Hard rules (always on)

1. **`settings.json` is generated — never hand-edit it.** Edit `internal/settings/settings.tmpl.json`
   (hooks: `internal/hookspec/hookspec.go`), then `make install`. Only `settings.local.json` is hand-edited.
2. **Secrets live only in the OS keyring, `env.d/secrets.env`, or a provider's `auth.command`** — never in
   `providers.json`, `settings.json`, a commit, or a log line.
3. **Commit messages carry NO AI-attribution footers** (`Co-Authored-By: Claude`,
   `Generated with Claude Code`, …) — this overrides any default that says to add them.
4. **Never `git stash` / `checkout` / `reset` / `restore` in the live checkout (`~/.claude`).** It is
   one checkout shared by parallel subagents and every live session. A linked worktree
   (`~/worktrees/<repo>/<slug>`, shared with Orca, via `EnterWorktree` or `/workflow:worktree`) is a separate checkout:
   branch there freely, merge back from the live tree (`git merge wt/<slug>`), never `checkout`/`reset`
   the live tree to it. `make install` and `claude-code` verbs still write to `~/.claude` from a
   worktree unless `CLAUDE_CONFIG_DIR` points at it.

## Behavioral guidelines

1. **Think before coding.** State assumptions; surface multiple interpretations instead of picking
   silently; name a simpler approach if one exists.
2. **Simplicity first.** Minimum code that solves the problem — no speculative features, no
   abstractions for single-use code, no handling for impossible states.
3. **Surgical changes.** Touch only what the request requires; match existing style; don't refactor
   adjacent code. Remove only orphans your change created; mention pre-existing dead code.
4. **Goal-driven execution.** Turn tasks into verifiable goals ("fix the bug" → "reproduce it, fix,
   re-run"). Loop until verified. → 1–4 in depth: `skills/evaluate-code/references/`.
5. **Terse comments.** 1 line max, ≤120 chars, per section; state the non-obvious "why", not history.
6. **Live check first; scoped tests during development; the full suite once.** Run the verb or flow
   the task is about before writing tests around it. Test only the packages you touched. `make test`
   runs once, when the work is complete — state what ran and that the suite did not.
   Every code dispatch carries the `copilot` test agent in the same message. → `rules/standards/quality.md`
7. **Propose look/behavior changes before applying them.** Name an unrequested side effect with
   numbers, prefer the narrowly scoped mechanism over a global knob, and let the user pick.
8. **Parallel by default; keep the main thread free.** For every code change the main thread plans and
   briefs, then dispatches `code-worker` (implements) ‖ `copilot` (repairs tests) in one message, in the
   background, and stays free for discussion, follow-ups and the live check. Inline only for a one-line
   edit. Brief completely up front; never re-steer a running agent. → `rules/standards/quality.md`

## How-to (task → command → depth)

| To… | Run | Then / depth |
| --- | --- | --- |
| Build and install (shims in `~/.claude/bin`, render `settings.json`) | `make install` (`claude-code install --dry-run` previews) | `README.md` → Install |
| Set up a box end to end (provider, logins or keys, models, vendor configs) | `claude-code configure [section] [--agent claude|codex|copilot|opencode|gemini] [--check]` (`--provider` = the model backend, `--agent` = one agent CLI) · `make bootstrap` | `README.md` → Setup |
| Adopt an existing `~/.claude`, wipe it for a fresh start, or scope a project's own `.claude/` | `claude-code install --setup adopt\|nuke\|workspace` · `scripts/bootstrap.sh` (fresh-box clone+adopt) | `README.md` → Setup modes |
| Render every config the harness owns (settings.json + codex/opencode/gemini) | `claude-code generate [--out f] [--check] [--dry-run]` | `README.md` → Setup |
| Sync VS Code's Machine settings + per-agent terminal keybindings to the harness | `claude-code vscode [--check\|--dry-run] [--dest f] [--keybindings-dest f]` | `README.md` → Setup |
| Tab-complete claude-code in bash | `claude-code completion --install` (done by `install`); `source <(claude-code completion)` now | `README.md` → Install |
| Check binaries, providers, credentials, router | `claude-code doctor` | every diagnostic verb takes `--format text\|json` (doctor, profile, usage, history log, mcp doctor, memory search) |
| See providers and credential state | `claude-code profile` (alias: `providers`) · `claude-code profile <name>` | `README.md` → Providers |
| Switch the session provider | `claude-code switch <provider> [--dry-run]` (alias: `use`; relaunch); one launch: `claude --provider <p>` | pin lands in `env.d/provider.env` |
| Pin models (`--dry-run` previews any pin change) | `claude-code models --pin opus=<id>` · `--pin haiku=openai:gpt-5-nano` (cross-provider slot) · `--pin all=<id>` · `--unpin` | env `HARNESS_<SLOT>_MODEL` beats the pin file |
| List what a provider serves | `claude-code models --live [--provider <p>]` | — |
| Add a gateway provider (corporate or self-hosted) | add it to `~/.claude/env.d/providers.json` with `routes` per dialect; copy `env.d/providers.example.json` | `README.md` → Providers |
| Set a credential | `claude-code secrets set [NAME]` (OS keyring; bare on a terminal = guided dialog; `--file` → `env.d/secrets.env`), or `auth.command`; also `secrets get\|status\|scan\|git\|doctor\|all` | `README.md` → Credentials |
| Run the model router | `claude-code router start\|status\|stop` (`serve --port N` in the foreground); `claude --router` forces it | `README.md` → Routing |
| Ask another agent (one shot) | `claude-code ask --agent <runner> "<task>"` (`--format json` = envelope); `ask --log` lists recent dispatches, `ask --retry <id>` replays one; `claude-code agents` lists runners | `skills/subagent-workflows/references/cross-model-agent-teams.md` |
| Sweep many files in one Gemini call (1M context) | `claude-code gemini bridge --dirs <d> [--index] [--diff] [--rubric <file>] "<task>"`; `gemini ask "<task>"` drives agy (the Antigravity CLI) | `commands/gemini/gemini.md`, `agents/gemini.md` |
| Project agents + this file into every vendor (codex, copilot, gemini, opencode; local render in `state/projections/`), link skills into `~/.agents/skills` | `claude-code agents sync [--vendor v] [--dry-run\|--check]` (also run by `install`) | `commands/harness/sync-agents.md`; `state/projections/` is generated + gitignored — edit `agents/`, `skills/`, this file; no AGENTS.md/GEMINI.md at this root (Claude Code would double-load it) |
| Launch codex/gemini/opencode/copilot on a provider | `claude-code run codex --provider <p> --model <m>` (or the bare shim); `--print-env` shows the wiring | `README.md` → Agents |
| Start an agent TUI on another tailnet box, detached | `claude-code dispatch --host <name> --agent claude\|codex\|opencode\|gemini\|copilot "<task>"` (tmux session, prints the attach command); `--list --host <h>` · `--attach --host <h> <session>` | `--dry-run` prints the ssh argv |
| Watch agents and dispatch from a phone, over the tailnet | `claude-code web [--addr ip:port] [--hosts h1,h2] [--days N]` (binds loopback or a 100.64.0.0/10 tailnet address only) | `internal/web`; CSRF token + Host-header allowlist guard every request |
| See every subagent's status, activity and vendor legs; drill into one | `claude-code agents tree [--session id] [--watch]` · `claude-code web view <agentId> [--follow]` | journal-backed; a `codex→claude` fallback is flagged on the leg row |
| Pane state of dispatched agents on a host | `claude-code dispatch --list --host <h> --status [--format json]` | working \| input-needed \| ready \| unknown \| gone; the web page caches the same |
| List, create, clean worktrees under `~/worktrees/<repo>/` | `claude-code worktree list [--format json]` · `create <name>` · `clean [--dry-run] <name>` · `clean-all [--yes]` | same engine as the worktree hooks; never forces a removal |
| Which browsers Claude in Chrome can drive; install the extension in another one | `claude-code web browsers` · `claude-code web setup --browser edge\|default [--dry-run]` | `doctor` + `configure browser` show the default's state; Safari/Firefox need a different control path |
| Add or retime a hook | implement in `internal/hooks/`, register in `internal/hookspec/hookspec.go`, `make install` | `README.md` → Hooks |
| Change permissions / env / statusLine | edit `internal/settings/settings.tmpl.json` → `claude-code generate` (alias: `settings`; prints) → `make install` | hard rule 1 |
| Check MCP server health, scope drift, inline secrets | `claude-code mcp doctor [--format json]` | `/harness:mcp-doctor` |
| Map the whole harness for an agent (verbs, providers, runners, hooks) | `claude-code describe [--format json\|text]` | credentials never appear, only whether one resolves |
| Sweep accumulated state (caches, run dirs, backups) | `claude-code clean [--list]` (applies by default; `--dry-run` previews; opt-in `--target projections\|sessions`) | credentials/settings never swept |
| See what acted: provider switches, pins, secret sets, safety blocks, router failovers | `claude-code doctor --audit [--days 7] [--format json]` (`state/harness/audit.jsonl`) | names only, never a value or a blocked command |
| Inspect or diff the shadow history of config (memory history: `git -C ~/.claude/memory log`) | `claude-code history snapshot\|log\|diff` (lives in `state/harness/history`, gitignored) | `env.d/secrets.env` is never snapshotted |
| Pick the statusline color palette · apply the whole look (VS Code, statusline, opencode, copilot, gemini) | `claude-code theme [--preview] [<palette>]` · `claude-code theme apply [--dry-run]` | `theme/README.md` — one JSON per tool, merged over its config |
| Recall or save session memory | `claude-code memory search <terms>` · `/memory:end` (auto via the Stop/SessionEnd harvest) · `memory init\|index\|path\|doctor\|stats\|timeline\|promote\|alias` | `README.md` → Memory bank |
| Keep the injected MEMORY.md small | `claude-code memory archive` (audit) · `--split 20 --apply` · `--dupes` · `archive <topic> --apply` / `--restore` | `README.md` → Memory bank |
| Test while developing | `go test ./internal/<pkg>` (one test: `-run '<Name>'`); `make lint` | full `make test` once at the end; `git push` runs the same gate via `.githooks/pre-push` (`--no-verify` bypasses) |
| Build a MEDIUM+ change (2+ files or needs tests) in ANY repo | `/code-workers` leaves on disjoint packages ‖ `copilot` test-repair agents, same message; `/workflow:orchestrate` for explore → plan → dispatch | `rules/workflow/master-workflow.md` → Phase 6 |
| Run a saved Workflow (only when the user asks for one) | `Workflow implement-and-verify` · `audit-fix-verify` · `gate-loop` · `review-funnel` · `dag` · `test-and-fix-fanout` · `vendor-leg` | `rules/workflow/master-workflow.md` → Saved Workflows |
| Lint a workflow script (model/effort pins, registry-hiding meta/throw) | `claude-code workflow lint [<root>]` · `workflow skeleton <pattern>` scaffolds one | `rules/workflow/master-workflow.md` → Saved Workflows · the `workflow-lint` PreToolUse hook auto-fills `model:'sonnet'` on an unpinned inline `agent()` and blocks only an ambiguous rewrite |
| Review a diff | `/code-review` (quality) ‖ `/security` | `skills/evaluate-code`, `skills/security-reviewer` |
| Fix failing tests | `/testing:test-and-fix` | `agents/copilot` |
| Capture a session's corrections into rules | `/harness:adapt` (auto at 85% context) | `rules/workflow/memory-consolidation.md` |
| Audit / extract / promote / save memory | `/reflection:review` · `:extract` · `:promote` · `:remember` · `:consolidate [--dream]` | `agents/opencode.md` |
| Second-opinion adversarial review on another model | `/codex:adversarial-review` (session-correlated handoff) · `/codex:dispatch` | `agents/codex.md` |
| Route a task to the cheapest capable leg | `/workflow:route --kind sweep\|review\|mechanical\|reasoning` | `rules/workflow/agents.md` |
| Scan the config surface / MCP for leaked credentials | `/harness:secrets-scan` · `/harness:mcp-doctor` | `rules/standards/security.md` |
| Tailnet: status, machines, connect, mint a key, exit node | `claude-code tailscale status\|machines\|connect\|key\|exit-node` · `make tailscale-cli` · configure row | `README.md` → Tailscale |

## Where things live

- `cmd/claude-code/` + `internal/` — the binary, stdlib-first; external SDKs boxed one package each.
  `internal/cli` is the verb table only; each verb's engine lives in its own package.
- `internal/providers/defaults.json` — built-in providers; `~/.claude/env.d/providers.json` overlays by name.
- `env.d/` — `provider.env` (pins, `switch`/`models`), `secrets.env` (credentials), `providers.json` (overlay),
  `custom.env` (launch env); untracked; each has a tracked `*.example` beside it.
- `agents/<name>.md` — subagents, flat (the layout VS Code Copilot Chat also reads); `name:` **must equal the file stem**.
- `commands/<namespace>/<command>.md` — slash commands. `workflows/*.js` — Workflow tool scripts.
- `rules/` — `standards/` (always-on) · `workflow/` · `software/<lang>/`; a rule without `paths:`
  frontmatter costs context every turn, so scope new ones (→ `rules/README.md`).
- `skills/<name>/SKILL.md` — on-demand references; untracked skill dirs are the user's own (symlinks into `~/.agents`, which Gemini reads natively), leave them.
- `skills/synced/` — claude.ai account skills Claude Code re-downloads each sync round; untracked, edit them on claude.ai.
- `plans/` — implementation plans (Claude Code's native `plansDirectory`); untracked, gitignored
  wholesale — this repo's own at the top, other repos' under `plans/<repo-slug>/`.
- `memory/` — the one memory store: a local-only git repo. `memory/index.md` is the generated catalog
  (router, never hand-edit); `memory/<repo>/` holds both native auto-memory (MEMORY.md + topics) and
  the six typed bank files per repo (harness itself = `memory/.claude/`); `memory/` top = global.
  `projects/<slug>/memory` is a symlink into it. Engine `internal/memory`, hooks `session-start` /
  `memory-recall` / `session-harvest[-end]`.
