---
paths:
  - "internal/hooks/**"
  - "internal/settings/hookspec.go"
---
# Hooks System

Hooks run as `claude-code hook <Event> [--<module>|--all]`. **`internal/settings/hookspec.go` is the only place a
hook is declared** (one `Spec` per invocation); the handler lives in `internal/hooks/`. Add, remove or retime: edit
both, then `make install` for projections; an existing Claude `settings.json` needs a hand edit
(`settings-management.md`).

## Groups and the dispatcher

`settings.Registry` lists groups in run order: `safety`, `permission`, `rtk`, `docs`, `dispatch`, `paths`, `memory`,
`format`, `review`, `worktree`, `codex`; `permission` and `codex` are agent-only, never in Claude settings. Settings
carry one dispatcher entry per event (`hook <Event>`) matching the union of its groups' matchers; it reads the payload
once, runs each group within its budget, recovers panics and merges results. `--<module>` runs one module; `--all`
runs every module on the event, skipping agent-only groups and applying each module's agent applicability.

- `safety` keeps its own process (`hook PreToolUse --safety-scan`) so a hung group can never delay or skip a block.
- No per-group switch: every group declared for an event runs. Dev override: launch with
  `HARNESS_HOOK_SKIP=<module>[,…]` or `all` (spares `safety-scan`/`permission-gate` unless named) in the hook's own
  env, never the vault; output notes each skip. `make lint SKIP=<kind>[,…]` drops lint kinds (`LINT_WHAT`).

## Applicability by agent

`settings.SpecsFor(agent, surface)` answers "which hooks run where". Surfaces: `native` (the agent's own hook file:
Claude settings, `~/.codex/hooks.json`, `~/.copilot/hooks`, `~/.gemini/config/hooks.json`), `hosted` (the harness runs
the module around a headless `opencode run`), `plugin` (opencode in-process). None of codex, copilot or agy has an ask
decision, so an ask blocks there.

| Agent · surface | Modules | Mode |
| --- | --- | --- |
| claude · native | every group that isn't agent-only | **block** (`safety` also asks); post/prompt hooks inject context |
| codex · native | `safety-scan`, `permission-gate`, `docs-*` gates, `paths-resolve`, `memory-recall`, `codex-*`; Stop `memory-update`; SessionEnd `memory-end` (3s cap) | **block**; `memory-end` advisory |
| copilot · native | `safety-scan`, `docs-*` gates, `paths-resolve`, `memory-recall`; Stop `memory-update`, `format-stop`; SessionEnd `memory-end` | PreToolUse **block**; the rest advisory |
| gemini (agy) · native | `safety-scan`, `permission-gate` | **block** |
| opencode · hosted | `Hosted` specs (docs, memory, format, review; no worktree notices) | **advisory** (`--pure` loads no plugin, so it is refused while a PreToolUse gate is bound); Stop and SessionEnd run only after a watched tool call |
| opencode · plugin | `safety-scan`, `permission-gate`, `docs-placement`, `docs-plan-gate`, `worktree-claim-check` | **block** (throws in `tool.execute.before`; the host then skips these) |

`permission-gate` judges each call with `Permissions.Decide` against `HARNESS_DISPATCH_PERMISSIONS`; how each agent
reaches it, and the gated Claude launches: `agent.md` → Bash deny via `permission-gate`.

## Event → hook matrix

Rows marked ¶ have full behavior text in `skills/subagent-workflows/references/hook-details.md`.

| Hook | Event | What it does |
| --- | --- | --- |
| `safety-scan` ¶ | PreToolUse (Bash) | **Block** destructive shapes, Bash writes to files the `Edit(...)` deny rules protect and history rewrites in a shared checkout; **ask** on Claude (block on codex/copilot, so never run it) for `git stash/checkout/switch/reset/restore/clean` under `~/.claude`, unstrippable AI-attribution footers and intent-dependent shapes; **rewrite** strippable footers and `; git push` after a gate (Claude/Codex, default and auto mode; copilot/agy/opencode deny with the fix; compound shapes deny or ask); non-conventional commits and catch-all `git add` are denied with the fix; blocks log to `doctor --audit` (reason, never the command) |
| `permission-gate` | PreToolUse (Bash\|Write\|Edit\|Read\|WebFetch\|Glob\|Grep) | **Block** a call outside the dispatch's `Permissions` (5s); `apply_patch` is judged as an Edit per file; agent-only |
| `rtk-rewrite` ¶ | PreToolUse (Bash) | Rewrites a command via `rtk rewrite` (never sets a permission decision); Claude sessions only; exact bytes: `rtk bypass <cmd>` |
| `docs-placement` | PreToolUse (Write\|Edit\|MultiEdit\|Bash) | Blocks a new `.md` at a repo root or under a gitignored `docs/`, naming the home (`docs-placement.md`); also checks `apply_patch` headers and Bash write targets |
| `docs-plan-gate` | PreToolUse (Write\|Edit) | Rewrites a new `*plan*.md` at a repo root or `docs/` to the dated plans home (denies with the fix where the host cannot rewrite); denies the first write to a native plan draft lacking the `docs/templates/plan.md` headings, listing them |
| `dispatch-effort` ¶ | PreToolUse (Agent\|Task\|Workflow) | Gates type (rewrites `coworker:<agent>` to `<agent>`, or denies on a `Run:` brief); pins model and effort by task tier for an unpinned type; claims slots, recalls context, labels the row; denies a `Run:` line naming a missing workspace or (with `--fallback none`) an unserved model |
| dispatch cap | within `dispatch-effort` | **24 in flight per available account** (a profile's `concurrency` overrides), machine-wide `24 × accounts` clamped to **4 × CPUs**; a blocked dispatch claims nothing, infra errors fail open; `doctor` shows `in-flight`. The description starts `<model>: <task>` (`claude-code agents label <type> --task FILE` prints it; a wrong label is rewritten, or denied once with the text where the host cannot rewrite) |
| `dispatch-failure` · `dispatch-workflow` · `dispatch-stop` ¶ | PostToolUseFailure · PostToolUse (Workflow) · SubagentStop | Retire the spawn's slots; `dispatch-stop` blocks a leaf's stop while its job still runs (≤2 h) and, for a `codex`/`copilot`/`gemini`/`opencode` leaf, up to 3 times without an `ask: ran on <agent> ·` line |
| `dispatch-handback` | PreToolUse (SubagentHandback) | Runs `dispatch-stop`'s two leaf gates on the hand-back call, which ends a background leaf before SubagentStop |
| `docs-plan-mirror` | PostToolUse (Write\|Edit) | Mirrors the undated draft to a dated, repo-tagged file from the first write (slug from `Suggested slug:`, else the H1); a shared lock serializes it |
| `docs-plan-save` | PostToolUse (ExitPlanMode) | Files approved text at the mirror destination, adds a missing checklist, verifies, removes the draft |
| `docs-plan-tick` / `docs-plan-reminder` ¶ | PostToolUse (Bash) / Stop | A landed commit ticks the plan tasks its `owns:` covers (`claude-code plan status`); partly landed tasks are reported once at Stop |
| `memory-update` ¶ | Stop | Context steward at ≥`HARNESS_CONTEXT_STEWARD_PCT` (default 75; `0` mutes the prompt), then the memory update; never blocks, a failed spawn prints a NOT-saved note naming `claude-code memory update` (SessionEnd/PreCompact retry) |
| `format-stop` | Stop | Formats this session's still-dirty edited files in its cwd subtree (≤50, newest first): repo `make format\|fmt` with a per-file variable, else the pyproject chain, else gofmt/ruff/prettier/shfmt |
| `review-simplicity` | PostToolUse (Write\|Edit) | Advisory: feeds back ≤8 comment, complexity and smell findings the edit introduced |
| `memory-start` | SessionStart | Injects a pointer to `memory/index.md` and this repo's focus; repairs `projects/<slug>/memory`; on `compact`/`clear` reports the compaction record and the newest handoff under 24 h (`memory-bank.md` → Who writes what) |
| `paths-resolve` | UserPromptSubmit | Reports existing paths in the prompt; translates Windows ⇄ `/mnt/…` on WSL |
| `memory-recall` | UserPromptSubmit | Injects up to 2 bank bodies plus 3 pointers within 1,500 chars, matching ≥2 prompt terms; logs hits to `access.log` |
| `codex-agent-route` · `codex-routing-result` · `codex-routing-start` · `codex-subagent-start` | codex agent events | Codex-only routing hooks; never in Claude settings |
| `memory-compact` | PreCompact | Dispatches a background update for uncovered edits and leaves a per-session record under `state/harness/compact/`; never blocks |
| `memory-end` | SessionEnd | After enough edits spawns `claude-code memory update`; the detached `memory watch` (`memory-bank.md`) |
| `worktree-create` / `worktree-remove` | WorktreeCreate / WorktreeRemove | Create at `~/worktrees/<repo>/<name>` (honors `worktree.baseRef`, seeds `.venv`/`.env`, opens ledger `memory/<repo>/worktrees/<name>.md`); remove refuses a dirty tree and `branch -d`s only a merged branch |
| `worktree-busy` ¶ | UserPromptSubmit | Vault env `HARNESS_WORKTREE` (`worktree-git.md` → Worktrees); drains this session's notices (≤5) and adds a divergence line in a diverged shared checkout |
| `worktree-claim` | PostToolUse (Write\|Edit\|MultiEdit\|Bash) | Appends written, staged and committed paths to `state/harness/hooks/claims/<pid>-<session>.jsonl`; never blocks |
| `worktree-claim-check` ¶ | PreToolUse (Write\|Edit\|MultiEdit\|Bash) | Notes before an edit that a dirty file is claimed by another live session or sibling leaf; asks on a staging or commit command that includes one (`-p` passes), blocks in a leaf; infra errors allow with a warning |

## Authoring a gate

Prose is the instruction; the hook is the guarantee. Every gate:

1. **Triggers on a checkable condition** — transcript facts or file state, never self-report.
2. **Blocks with instructions and the evidence contract** — what artifact proves compliance.
3. **Releases only on evidence** — mtimes, receipts, exit codes.
4. **Caps and fails open** — a Stop gate that can trap a session needs a cap and a visible fail-open; a deny-one-call
   gate (`docs-placement`) rejects the input and lets the model retry. Infra errors fail open, except workflow stage
   route-preview errors; a `Blocks` hook that panics, times out (0.5 s before the host's timeout) or gets an empty or
   unparseable payload blocks.

Test the block path, the evidence-release path and the fail-open cap. Probe a live guard with a script that reads
payloads from a data file (quoted payloads in the probe command trip the guard itself) and tally allow, ask and block
separately. Permission guardrails live in `settings.json`'s `permissions` block (`internal/settings/template.json`;
`ask` beats `allow`).
