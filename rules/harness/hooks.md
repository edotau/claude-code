---
paths:
  - "internal/hooks/**"
---
# Hooks System

Hooks run as `claude-code hook <Event> [--<module>|--all]`. **`internal/settings/hookspec.go` is the only place a
hook is declared** (one `Spec` per invocation); the handler lives in `internal/hooks/`. Add, remove or retime: edit
both, then `make install`. See `settings-management.md`.

## Groups and the dispatcher

`settings.Registry` lists groups in execution order: `safety`, `rtk`, `docs`, `dispatch`, `paths`, `memory`,
`format`, `review`, `worktree`, `codex` (codex-only, never rendered into Claude settings). Settings carry **one
dispatcher entry per event** (`hook <Event>`) matching the union of its groups' matchers; it reads the payload once,
runs each enabled group within its budget, recovers panics and merges results. `--<module>` runs one module; `--all`
runs
every module on the event, the gate included.

- **`safety` keeps its own process** (`hook PreToolUse --safety-scan`) so a hung group can never delay or skip a
  block, and it cannot be switched off.
- **Switches:** any other group turns off with `"<group>": false` under `harnessHooks` in the vault overlay
  (`claude-code vault edit`); an unknown group or a gate group is rejected.
- **Vendors:** codex and copilot run only `safety-scan`, `docs-placement`, `docs-plan-gate`, `paths-resolve` and
  `memory-recall`, each as `hook <Event> --<module>`. Neither has an ask decision, so a safety ask blocks there.

## Event → hook matrix

| Hook | Event | What it does |
| --- | --- | --- |
| `safety-scan` | PreToolUse (Bash) | **Block** destructive shapes (`rm -rf /`, force-push to main, `curl \| sh`, `git stash/checkout/reset/restore/clean` at `~/.claude` — linked worktrees exempt — and a commit with an AI-attribution footer); **ask** on intent-dependent shapes (`sudo`, `DROP TABLE`) naming the safer alternative; **warn** only prints. Pipe, exfil and SQL shapes ignore inert text (commit/tag/`gh` messages, `echo`/`printf`/`grep`/`rg` arguments, file-only `cat`/`tee` heredoc bodies) unless it is piped on or holds a `$(…)`. Blocks log to `claude-code doctor --audit` by reason and pattern, never the command |
| `rtk-rewrite` | PreToolUse (Bash) | Rewrites a command via `rtk rewrite` (chains included; exit 0/3 with changed output; never sets a permission decision) and runs the pinned `bin/rtk` (else PATH); Claude sessions only. Vetoes: already wrapped, any `git` in a linked worktree, any `git commit`, machine-readable flags (`--porcelain`, `--json`), value subcommands (`rev-parse`). Exact bytes: `bin/rtk bypass <cmd>`; `HARNESS_RTK_BIN` overrides. A bare `rtk hook claude` entry in settings is stale and removed on `make install` |
| `docs-placement` | PreToolUse (Write\|Edit) | Blocks a new `.md` at a repo root or under a gitignored `docs/`, naming the home (`docs-placement.md`); also checks `apply_patch` headers |
| `docs-plan-gate` | PreToolUse (Write\|Edit) | Denies the first write to a native plan draft lacking `# Plan:`, `## Context`, `## Phases`, `## Tasks` from `agents/Plan.md`; the denial pastes the skeleton |
| `dispatch-effort` | PreToolUse (Agent\|Task\|Workflow) | In order: cap, model pin, slot claim. A brief's `Profile: <name>` moves the dispatch to `coworker`, labels the row, puts the `ask --profile` command in the prompt and denies an unknown name. Fills an UNSET model from a local classifier (low→haiku, else sonnet); an explicit `model:` (dispatch or frontmatter, `inherit` included) always wins. At ≥90% Anthropic plan pressure downshifts an unset model one step; at ≥97% moves `code-worker`/`Explore` to a coworker (`HARNESS_ROUTE_SUBAGENTS=0` disables). Appends ≤3 memory recall pointers. Workflows: pins `model: 'sonnet'` on every unpinned `agent()` (inline or `scriptPath`), redirects a by-name call to its `scriptPath` (registry read once at session start), blocks an unrewritable options argument or a bad pin literal |
| (dispatch cap) | within `dispatch-effort` | **12 in flight per available account** (a profile's `concurrency` overrides), machine-wide `12 × accounts` clamped to **4 × CPUs**, test agents included. A blocked dispatch claims nothing; a Workflow claims one slot per `agent()` site; infra errors fail open. `claude-code doctor` shows the live `in-flight` count. Describe a dispatch by its task alone; the row shows the leaf type and `dispatch-effort` adds who runs it (`skills/subagent-workflows/SKILL.md` → Leaf types) |
| `dispatch-failure` · `dispatch-workflow` · `dispatch-stop` | PostToolUseFailure · PostToolUse (Workflow) · SubagentStop | Retire the spawn's slots. `dispatch-stop` first blocks a leaf's stop (up to 2 hours from the dispatch's start, then a visible fail-open) while a `job <id> started` its own transcript printed is still running, naming `claude-code agents tail -f <id>`; a blocked stop retires nothing |
| `docs-plan-mirror` | PostToolUse (Write\|Edit) | Mirrors the undated draft to a dated, repo-tagged file from the first write (slug from `Suggested slug:`, else the H1); edits keep destination and date |
| `docs-plan-save` | PostToolUse (ExitPlanMode) | Files approved text at the mirror destination (or files a response-only plan), adds a missing checklist, verifies, removes the draft |
| `docs-plan-skeleton` / `-prompt` | PostToolUse (EnterPlanMode) / UserPromptSubmit | Injects the `agents/Plan.md` skeleton when plan mode starts, and once per plan on a plan-mode prompt |
| `docs-plan-tick` / `docs-plan-reminder` | PostToolUse (Bash) / Stop | A landed commit ticks the plan tasks its `owns:` covers (`claude-code plan status`); partly landed tasks are reported once at Stop |
| `memory-update` | Stop | Context checkpoint first: once per session at ≥85% context it blocks Stop so the model writes a handoff (`claude-code memory update`). Then the memory update, weighing edits and user turns (2 turns = 1 edit) |
| `format-stop` | Stop | Formats files edited this turn: the repo's `make format\|fmt` if it takes a per-file variable, else the pyproject python chain, else gofmt/ruff/prettier/shfmt; a repo with its own pre-commit switches the group off |
| `review-simplicity` | PostToolUse (Write\|Edit) | Advisory, never blocks: feeds back ≤8 comment, complexity and smell findings the edit introduced (not ones already in the file) |
| `memory-start` | SessionStart | Injects `memory/index.md` + this repo's focus (Current Focus/Blockers/Next Steps, In Progress); repairs `projects/<slug>/memory` |
| `paths-resolve` | UserPromptSubmit | Reports existing paths in the prompt; translates Windows ⇄ `/mnt/…` on WSL |
| `memory-recall` | UserPromptSubmit | Injects ≤3 bank blocks matching ≥2 prompt terms (plus a repo store the prompt names); skips blocks in context; logs hits to `access.log` (the hot/cold signal for `memory review`) |
| `codex-agent-route` · `codex-routing-result` · `codex-routing-start` · `codex-subagent-start` | codex vendor events | Codex-only routing hooks; never rendered into Claude settings |
| `memory-compact` | PreCompact | Dispatches a background update for uncovered edits; never blocks |
| `memory-end` | SessionEnd | After enough edits spawns `claude-code memory update`; the first dispatch claims one detached `memory watch` that updates after 10 quiet minutes, so a crash loses nothing |
| `worktree-create` / `worktree-remove` | WorktreeCreate / WorktreeRemove | Create at `~/worktrees/<repo>/<name>` (honors `worktree.baseRef`, seeds `.venv`/`.env`, opens ledger `memory/<repo>/worktrees/<name>.md`); remove refuses a dirty tree and `branch -d`s only a merged branch |
| `worktree-mode` / `worktree-busy` | SessionStart / UserPromptSubmit | Vault env `HARNESS_WORKTREE`: unset asks once at session start and when a peer joins the checkout (not this session's own tree, a linked worktree, or an `agy --hub` daemon); `off`/`false`/`0` is silent. Never creates a worktree; no yes means keep working in place |

## Authoring a gate

Prose is the instruction; the hook is the guarantee. Every gate:

1. **Triggers on a checkable condition** — transcript facts or file state, never self-report.
2. **Blocks with instructions and the evidence contract** — what artifact proves compliance.
3. **Releases only on evidence** — mtimes, receipts, exit codes.
4. **Caps and fails open** — a Stop gate that can trap a session needs a cap and a visible fail-open; a
   deny-one-call gate (e.g. `docs-placement`) rejects the input and lets the model retry. Infra errors fail open.

Test the block path, the evidence-release path and the fail-open cap. Permission guardrails live in the defaults'
`permissions` block (`ask` beats `allow`).
