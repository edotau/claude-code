---
paths:
  - "internal/hooks/**"
  - "internal/settings/hookspec.go"
---
# Hooks System

Hooks run as `claude-code hook <Event> [--<module>|--all]`. **`internal/settings/hookspec.go` is the only place a
hook is declared** (one `Spec` per invocation); the handler lives in `internal/hooks/`. Add, remove or retime: edit
both, then `make install` for projections. Existing Claude settings require a hand edit or explicit
`claude-code configure --restore`; see `settings-management.md`.

## Groups and the dispatcher

`settings.Registry` lists groups in run order: `safety`, `permission`, `rtk`, `docs`, `dispatch`, `paths`, `memory`,
`format`, `review`, `worktree`, `codex`; `permission` and `codex` are agent-only, never in Claude settings.
Settings carry **one dispatcher entry per event** (`hook <Event>`) matching the union of its groups' matchers; it
reads the payload once, runs each group within its budget, recovers panics and merges results. `--<module>`
runs one module; `--all` runs every module on the event, gates included, but still skips agent-only
groups and applies each module's agent applicability.

- **`safety` keeps its own process** (`hook PreToolUse --safety-scan`) so a hung group can never delay or skip a
  block.
- **No per-group switch:** every group declared for an event runs.
- **Agents:** codex, copilot and agy run only the modules in the table below, each as `hook <Event> --<module>`.
  None has an ask decision, so an ask blocks there.

## Applicability by agent

`settings.SpecsFor(agent, surface)` is the one answer to "which hooks run where"; surfaces: `native` (the agent's
own hook file: Claude settings, `~/.codex/hooks.json`, `~/.copilot/hooks`,
`~/.gemini/config/hooks.json`), `hosted` (the
harness runs the module around a headless `opencode run`), `plugin` (opencode in-process plugin).

| Agent · surface | Modules | Mode |
| --- | --- | --- |
| claude · native | every group that isn't agent-only (so not `permission` or `codex`) | **block** (`safety` also asks); post/prompt hooks inject context |
| codex · native | `safety-scan`, `permission-gate`, `docs-*` gates, `paths-resolve`, `memory-recall`, `codex-*`; Stop `memory-update`; SessionEnd `memory-end` (codex caps it at 3s) | **block** (an ask blocks); `memory-end` advisory |
| copilot · native | `safety-scan`, `docs-*` gates, `paths-resolve`, `memory-recall`; Stop `memory-update`, `format-stop`; SessionEnd `memory-end` | PreToolUse **block** (an ask blocks); the rest advisory |
| gemini (agy) · native | `safety-scan`, `permission-gate` | **block** (an ask blocks) |
| opencode · hosted | `Hosted` specs (docs, memory, format, review; no worktree notices) | all **advisory** (`--pure` loads no plugin, so it is refused while any PreToolUse gate is bound); Stop and SessionEnd run only after a watched tool call |
| opencode · plugin | `safety-scan`, `permission-gate`, `docs-placement`, `docs-plan-gate`, `worktree-claim-check` | **block** (throws in `tool.execute.before`, headless and TUI; the host then skips these) |

The `permission-gate` module (AgentOnly `permission` group) judges each call with `Permissions.Decide` against
`HARNESS_DISPATCH_PERMISSIONS`, covering `bash -c`: opencode runs it from the plugin, codex and agy natively. Copilot
`ask` runs through the SDK, whose session loads `~/.copilot/hooks/*.json`, so the native row covers it. Skills,
instruction sources and the Bash-deny path: `agent.md` → Skills and instructions parity. Claude runs it only in a
gated launch (memory jobs, `--read-only`, `--tools` off Anthropic): `claudeCmd` injects it into that child's one
`--settings` (never `settings.json`), under `bypassPermissions`, `agent.md` → Memory jobs.

## Event → hook matrix

| Hook | Event | What it does |
| --- | --- | --- |
| `safety-scan` | PreToolUse (Bash) | **Block** destructive shapes (`rm -rf /`/`$HOME`, fork bomb, `curl | sh`, `base64 -d | sh`), Bash writes to files the `Edit(...)` deny rules protect, and `commit --amend`/`rebase`/`reset <commit>` in a shared checkout; **ask** on Claude (block on codex/copilot) for `git stash/checkout/switch/reset/restore/clean` under `~/.claude`, AI-attribution commit footers and intent-dependent shapes (`sudo`, `DROP TABLE`, `rm -rf .git`, main/master force-push); inert text is ignored, any `<<` scans heredocs; blocks log to `claude-code doctor --audit` (reason and pattern, never the command). Full shape list: `skills/subagent-workflows/references/hook-details.md` |
| `permission-gate` | PreToolUse (Bash\|Write\|Edit\|Read\|WebFetch\|Glob\|Grep) | **Block** a call outside the dispatch's `Permissions` (5s), codex's `apply_patch` judged as an Edit on each file its patch headers name; agent-only: codex, gemini, opencode plugin, never in Claude settings |
| `rtk-rewrite` | PreToolUse (Bash) | Rewrites a command via `rtk rewrite` (exit 0/3 with changed output; never sets a permission decision) using the pinned `bin/rtk` (else PATH); Claude sessions only. Vetoes: already wrapped, any `git` in a linked worktree, any `git commit`, machine-readable flags, `rev-parse`, `go vet`. Exact bytes: `rtk bypass <cmd>`; `HARNESS_RTK_BIN` overrides; a bare `rtk hook claude` settings entry is stale. Full text: `skills/subagent-workflows/references/hook-details.md` |
| `docs-placement` | PreToolUse (Write\|Edit\|MultiEdit\|Bash) | Blocks a new `.md` at a repo root or under a gitignored `docs/`, naming the home (`docs-placement.md`); also checks `apply_patch` headers and Bash write targets (`sed -i`, redirects, `tee`, `cp`/`mv`) |
| `docs-plan-gate` | PreToolUse (Write\|Edit) | Denies the first write to a native plan draft lacking `# Plan:`, `## Context`, `## Phases`, `## Tasks` from `docs/templates/plan.md`; the denial lists the headings and names the template path |
| `dispatch-effort` | PreToolUse (Agent\|Task\|Workflow) | Gates type; pins model and effort from the task's tier for a type with no `model:` line and no explicit model (never for a `Run: claude-code agents ask` brief); claims slots, recalls context, labels the row. Denies a `Run:` line naming a missing Databricks workspace or (with `--fallback none`) a model its fresh cached listing does not serve. Brief routing: `skills/subagent-workflows/SKILL.md` → Handoff contract; code: `internal/hooks/agents/effort.go`, `internal/hooks/quality/workflowlint.go`. Full text and auto-mode caveats: `skills/subagent-workflows/references/hook-details.md` |
| (dispatch cap) | within `dispatch-effort` | **24 in flight per available account** (a profile's `concurrency` overrides), machine-wide `24 × accounts` clamped to **4 × CPUs**, test agents included. A blocked dispatch claims nothing; a Workflow claims one slot per `agent()` site; infra errors fail open. `claude-code doctor` shows the live `in-flight` count. The description starts `<model>: <task>` (in auto mode the hook denies once naming the exact text to resend, else it rewrites; `claude-code agents label <type> --task FILE` prints the same prefix); the row shows the leaf type, which is the agent (`claude`, `copilot`, `codex`…) (`skills/subagent-workflows/SKILL.md` → Leaf types) |
| `dispatch-failure` · `dispatch-workflow` · `dispatch-stop` | PostToolUseFailure · PostToolUse (Workflow) · SubagentStop | Retire the spawn's slots. `dispatch-stop` blocks a leaf's stop (≤2 hours, then a visible fail-open) while a `job <id> started` its transcript printed still runs, naming `claude-code agents result -f <id>`; a `codex`/`copilot`/`gemini`/`opencode` leaf is blocked up to 3 times at a stop with no `ask: ran on <agent> ·` line or `ran_on` field (4th passes visibly; `claude` leaves and `Agent: in-context` exempt); an unblocked stop kills the leaf's leftover background shells. Full text: `skills/subagent-workflows/references/hook-details.md` |
| `dispatch-handback` | PreToolUse (SubagentHandback) | Runs `dispatch-stop`'s two leaf gates (ran-on: `codex\|copilot\|gemini\|opencode`) on the hand-back call, which ends a background leaf before SubagentStop can block it: a live dispatch denies until it ends (2 h cap); a missing ran-on line denies up to 3 times |
| `docs-plan-mirror` | PostToolUse (Write\|Edit) | Mirrors the undated draft to a dated, repo-tagged file from the first write (slug from `Suggested slug:`, else the H1); edits keep destination and date. A shared mirror lock serializes destination selection and writing; a failed destination record drops the empty claim and reports the error |
| `docs-plan-save` | PostToolUse (ExitPlanMode) | Files approved text at the mirror destination (or files a response-only plan), adds a missing checklist, verifies, removes the draft |
| `docs-plan-tick` / `docs-plan-reminder` | PostToolUse (Bash) / Stop | A landed commit ticks the plan tasks its `owns:` covers (`claude-code plan status`) in the newest open plan it touches; `git commit -q` prints no sha, so it ticks nothing (`plan tick --range`); partly landed tasks are reported once at Stop under one shared partials lock; `claude-code clean` sweeps `*.partial.lock` files older than one day. Full text: `skills/subagent-workflows/references/hook-details.md` |
| `memory-update` | Stop | Context steward first: at ≥`HARNESS_CONTEXT_STEWARD_PCT` context (default 75; `0` mutes only the prompt) it spawns a detached `claude-code memory update` and asks for a handoff then `/compact`, once per crossing, never blocking Stop. Then the memory update; with no spawn possible it blocks ≤3 times, then prints once that the update was NOT saved. Full text: `skills/subagent-workflows/references/hook-details.md` |
| `format-stop` | Stop | Formats files this session edited within its cwd subtree that are still dirty and unformatted, at most 50 per Stop, newest file mtime first: the repo's `make format\|fmt` if it takes a per-file variable, else the pyproject python chain, else gofmt/ruff/prettier (only with a prettier config or `package.json` entry)/shfmt |
| `review-simplicity` | PostToolUse (Write\|Edit) | Advisory, never blocks: feeds back ≤8 comment, complexity and smell findings the edit introduced (not ones already in the file) |
| `memory-start` | SessionStart | Injects a pointer to `memory/index.md` + this repo's focus; repairs `projects/<slug>/memory`; bank scaffolding, the daily `memory dream` and the `memory catchup` sweep: `rules/workflows/memory-bank.md` → Who writes what; on `compact`/`clear` it also reports the compaction record and the newest handoff under 24 h old (user line and transcript) |
| `paths-resolve` | UserPromptSubmit | Reports existing paths in the prompt; translates Windows ⇄ `/mnt/…` on WSL |
| `memory-recall` | UserPromptSubmit | Injects up to 2 bank bodies plus 3 pointers within 1,500 chars, matching ≥2 prompt terms (plus a repo store the prompt names); skips blocks in context; logs hits to `access.log` (the hot/cold signal for `memory review`) |
| `codex-agent-route` · `codex-routing-result` · `codex-routing-start` · `codex-subagent-start` | codex agent events | Codex-only routing hooks; never rendered into Claude settings |
| `memory-compact` | PreCompact | Dispatches a background update for uncovered edits and leaves a per-session record (trigger, context %, time) under `state/harness/compact/` for the next start; never blocks |
| `memory-end` | SessionEnd | After enough edits spawns `claude-code memory update`; the detached `memory watch`: `rules/workflows/memory-bank.md` |
| `worktree-create` / `worktree-remove` | WorktreeCreate / WorktreeRemove | Create at `~/worktrees/<repo>/<name>` (honors `worktree.baseRef`, seeds `.venv`/`.env`, opens ledger `memory/<repo>/worktrees/<name>.md`); remove refuses a dirty tree and `branch -d`s only a merged branch |
| `worktree-busy` | UserPromptSubmit | Vault env `HARNESS_WORKTREE`: unset asks when a peer joins the checkout; `off`/`false`/`0` is silent. Never creates a worktree; no yes means keep working in place. Also drains this session's notices (`state/harness/notices/`, ≤5) and adds one line per HEAD/upstream pair in a diverged shared checkout, whatever the mode. Full text: `skills/subagent-workflows/references/hook-details.md` |
| `worktree-claim` | PostToolUse (Write\|Edit\|MultiEdit\|Bash) | Appends each written path (Bash: `sed -i`, redirect, `tee`, `cp`/`mv` targets), each path a `git add` left staged, or each sha a landed `git commit` printed, to `state/harness/hooks/claims/<pid>-<session>.jsonl`; never blocks |
| `worktree-claim-check` | PreToolUse (Write\|Edit\|MultiEdit\|Bash) | Once per file, notes before an edit that a dirty file is claimed by another live session or sibling leaf and not committed since; asks on a `git add`/`commit` that stages one (`-p` passes), blocks in a leaf; an infra error allows with a warning; any `git` command also gets the drained notices and divergence line. Full text: `skills/subagent-workflows/references/hook-details.md` |

## Authoring a gate

Prose is the instruction; the hook is the guarantee. Every gate:

1. **Triggers on a checkable condition** — transcript facts or file state, never self-report.
2. **Blocks with instructions and the evidence contract** — what artifact proves compliance.
3. **Releases only on evidence** — mtimes, receipts, exit codes.
4. **Caps and fails open** — a Stop gate that can trap a session needs a cap and a visible fail-open; a
   deny-one-call gate (e.g. `docs-placement`) rejects the input and lets the model retry. Infra errors generally
   fail open; workflow stage route-preview errors block, including registry/credential failures. A blocking
   (`Blocks`) hook that panics, times out (in-process, 0.5 s before the host's timeout) or gets an empty or
   unparseable payload blocks.

Test the block path, the evidence-release path and the fail-open cap. Probe a live guard with a script that reads
payloads from a data file (quoted payloads in the probe command trip the guard itself) and tally allow, ask and
block separately. Permission guardrails live in `settings.json`'s
`permissions` block (seeded from `internal/settings/template.json`; `ask` beats `allow`).
