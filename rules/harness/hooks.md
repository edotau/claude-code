---
paths:
  - "internal/hooks/**"
  - "internal/settings/hookspec.go"
---
# Hooks System

Hooks run as `claude-code hook <Event> [--<module>|--all|--async]`. **`internal/settings/hookspec.go` is the only
place a hook is declared** (one `Spec` per invocation); handlers live in `internal/hooks/`. To add, remove or retime
one, edit both and run `make install` for projections; an existing Claude `settings.json` needs a hand edit
(`settings-management.md`). `claude-code agents sync` re-renders each agent's native hook and config files;
`agents sync --check` exits 1 when one is stale.

## Groups and the dispatcher

`settings.Registry` lists the groups in run order; `permission` and `codex` are agent-only, never in Claude settings.

| Group | Does |
| --- | --- |
| `safety` | Bash safety gate (block / ask / warn tiers) |
| `permission` | Agent-only (codex, gemini, opencode): judges each call against the dispatch's permissions |
| `rtk` | Routes noisy command output through rtk |
| `docs` | Markdown placement; plan template, dated plan copies, plan-task ticks |
| `dispatch` | Concurrency cap, subagent model fill, memory recall on spawn and every `agents ask` |
| `paths` | Resolves Windows ⇄ `/mnt/…` paths in prompts |
| `memory` | Focus and recall at start/prompt; steward handoff and update at Stop/SessionEnd/PreCompact |
| `format` | Formats this session's still-dirty edited files |
| `review` | Advisory `review-simplicity` finding after each edit |
| `worktree` | Creates/removes under `~/worktrees/<repo>/`; ledger, claims, notices |
| `codex` | Codex only: child-agent routing and its log |

- **One synchronous entry per event** (`hook <Event>`, the union of its groups' matchers) reads the payload once, runs
  each group within its budget, recovers panics and merges results. `--<module>` runs one module; `--all` runs every
  module on the event except agent-only groups, applying agent applicability.
- **`safety` has its own process** (`hook PreToolUse --safety-scan`), so a hung group never delays or skips a block.
- **Async entry** (`hook PostToolUse --async`) runs the Bash side of `docs-plan-tick` and `worktree-claim`: the tool
  returns at once and the note reaches the model a turn later. A `settings.json` without that entry keeps them
  synchronous; a headless `claude -p` that exits first kills the run.
- **No per-group switch**: every declared group runs. Dev override, one launch only: `HARNESS_HOOK_SKIP=<module>[,…]`
  or `all` in the hook's own env (`all` spares `safety-scan` and `permission-gate` unless named); output notes each
  skip. Fix a misfiring module narrowly; delete an unwanted one (spec and handler), never skip it from the vault.

**Telemetry**: each module run appends one row to `state/harness/hooks/telemetry-YYYYMMDD.jsonl` (14 days, capped):
event, module, outcome, ms, 8-char session id, tool, and when a decision fired a `rule` and a `verdict` (`block`,
`ask`, `rewrite`, `held`, `refused`, `passed`); never command text, a path, a brief or a credential.
`claude-code doctor` reports seven days of fires, denies, budget overruns and p95 duration per module.

## Applicability by agent

`settings.SpecsFor(agent, surface)` answers "which hooks run where". Surfaces: `native` (the agent's own hook file:
Claude settings, `~/.codex/hooks.json`, `~/.copilot/hooks`, `~/.gemini/config/hooks.json`), `hosted` (the harness
runs the module around a headless `opencode run`), `plugin` (opencode in-process). Codex, copilot and agy have no ask
decision, so an ask denies there; their other hooks are advisory unless noted.

| Agent · surface | Modules | Mode |
| --- | --- | --- |
| claude · native | every group that isn't agent-only | **block** (`safety` asks only on destructive SQL and script writes); post/prompt hooks inject context |
| codex · native | `safety-scan`, `permission-gate`, `docs-placement`, `worktree-claim-check`, `paths-resolve`, `memory-recall`, `codex-*`; PostToolUse `worktree-claim` (`apply_patch` too); SessionStart `memory-start`; Stop `memory-update`; SessionEnd `memory-end` (3s cap) | **block** |
| copilot · native | `safety-scan`, `docs-placement`, `worktree-claim-check`, `paths-resolve`, `memory-recall`; PostToolUse `worktree-claim`; sessionStart `memory-start`; Stop `memory-update`, `format-stop`; SessionEnd `memory-end` | PreToolUse **block** |
| gemini (agy) · native | `safety-scan`, `permission-gate`, `docs-placement`, `worktree-claim-check`; PostToolUse `docs-plan-mirror`, `docs-plan-tick`, `review-simplicity`, `worktree-claim`; Stop `docs-plan-reminder`, `memory-update`, `format-stop`; SessionStart `memory-start` | PreToolUse **block**; SessionStart injects a system step; Stop block = `continue`; PostToolUse output is `{}`, so its notes never reach the model |
| opencode · hosted | `Hosted` specs (docs, paths, memory, format, review; `safety-scan` and `docs-placement` are declared hosted but fire only through the plugin; no worktree notices) | **advisory**; `--pure` loads no plugin, so it is refused while a PreToolUse gate is bound; Stop and SessionEnd run only after a watched tool call |
| opencode · plugin | `safety-scan`, `permission-gate`, `docs-placement`, `worktree-claim-check`; `worktree-claim` in `tool.execute.after` | **block** (throws in `tool.execute.before`; the host then skips these); the claim is a detached, non-blocking spawn per edit |

`permission-gate` judges each call with `Permissions.Decide` against `HARNESS_DISPATCH_PERMISSIONS`; how each agent
reaches it, and the gated Claude launches: `agent.md` → Bash deny via `permission-gate`.

## Event → hook matrix

Rows marked ¶ have full behavior in `skills/subagent-workflows/references/hook-details.md` under the hook's heading.

`safety-scan` enforces the Git guards in `CLAUDE.md` hard rule 4 and `rules/standards/worktree-git.md`.
It checks each call in order:

1. The code floor blocks every agent: the block shapes (except `sudo` and settings-rule writes) and attribution
   footers in commits, merges and tags.
2. Deny rows come from `~/.claude/settings.json` (template fallback, no other file); `sudo` follows `Bash(sudo:*)`.
3. For ask rows, Claude asks. An agent without ask follows the dispatch's `--allow`/`--deny`; if neither matches,
   it blocks and names `--allow '<rule>'`.
4. Allow rows apply only to main. A match skips the main-session rules, SQL ask and softWarn.
5. After built-ins, `agentBashAllow` allows a leaf's block or ask except `Result.Floor` (code floor and settings deny).

`Edit(...)` deny/ask globs also guard Bash and interpreter writes (`//` absolute, `~/`, `/` = `~/.claude/`, `**/`,
relative = cwd). A leaf cannot write `settings.json` or `settings.local.json` via Bash. Intent rules match invocations,
not data. Blocks name the allowed route and the owner's `!`; `claude-code doctor --audit` logs no command text.

| Hook | Event | What it does |
| --- | --- | --- |
| `safety-scan` ¶ | PreToolUse (Bash) | Block or ask in the order above |
| `permission-gate` | PreToolUse (Bash\|Write\|Edit\|Read\|WebFetch\|Glob\|Grep) | **Block** a call outside the dispatch's `Permissions` (5s); `apply_patch` is judged as an Edit per file; agent-only |
| `rtk-rewrite` ¶ | PreToolUse (Bash) | `rtk rewrite`s a command, never setting a permission decision; Claude only, not in auto mode; exact bytes: `rtk bypass <cmd>` |
| `docs-placement` ¶ | PreToolUse (Write\|Edit\|MultiEdit\|Bash) | **Block** a new `.md` at a repo root or in a gitignored `docs/`, naming the home (`docs-placement.md`); a leaf's Bash block becomes allow + note (Write/Edit stay blocked unless `--permissions allow`); redirects a new `*plan*.md` to the dated plans home; a first plan write needs the `docs/templates/plan.md` headings |
| `dispatch-effort` ¶ | PreToolUse (Agent\|Task\|Workflow) | Gates the type (`coworker:<agent>` → `<agent>`), pins model and effort by `agent.TaskTier` for an unpinned type, claims slots, labels, adds the recall block to every brief and dispatched `agents ask` (`withRecall`), all in one `updatedInput`; **fixes** a `Run:` line's unknown flag, bad value, positional agent name or `--task-file -` in the same `updatedInput` when one fix is certain, else notes it; **denies** a `Run:` line naming a missing workspace or (`--fallback none`) an unserved model |
| dispatch cap | within `dispatch-effort` | **24 in flight per available account** (a profile's `concurrency` overrides), machine-wide `24 × accounts` clamped to **4 × CPUs**; a blocked dispatch claims nothing; infra errors fail open; `doctor` shows `in-flight`. Description `<model>: <task>`, the model the run resolves (`claude-code agents label <type> --run`); a wrong label is rewritten, never denied |
| `dispatch-failure` · `dispatch-workflow` · `dispatch-stop` ¶ | PostToolUseFailure · PostToolUse (Workflow\|Bash) · SubagentStop | Retire the spawn's slots; `dispatch-stop` holds a leaf's stop while its job runs (≤2 h), runs a never-run `Run:` line itself, passes relay work with a note, saves a held report |
| `dispatch-handback` ¶ | PreToolUse (SubagentHandback\|Bash) | `dispatch-stop`'s leaf gates on the hand-back: waits ≤13 s for a running job, attaches each finished result with its receipt, else holds naming `claude-code agents result -f <id>`; on Bash, refuses duplicate polls and limits a relay's commands; on a hand-back naming a plan task (dispatch `plan_task` or the brief's `Task:`), runs its allowlisted `verify:` plus the scoped gate on its owned dirty files within the budget and puts `verify <id>: …` lines on top of the message (never holds it) |
| `docs-plan-mirror` | PostToolUse (Write\|Edit) | Mirrors the undated draft to a dated, repo-tagged file from the first write (slug: `Suggested slug:`, else the H1), under a shared lock |
| `docs-plan-save` | PostToolUse (ExitPlanMode) | Files approved text at the mirror destination, adds a missing checklist, verifies, removes the draft |
| `docs-plan-tick` / `docs-plan-reminder` ¶ | PostToolUse (Bash, async) / Stop | The main session's verified commit on main starts one detached `plan tick --verify` per plan for the tasks its `Task:` trailers name; it ticks each whose `verify:` and scoped gate pass on that sha in a scratch checkout and posts a notice; red stays open with the failing command and tail (coverage never ticks), noting `owns:` files left out (trailer, main and lane rules: `worktree-git.md` rule 3); `docs-plan-reminder` only hosts the Stop watchdog |
| `memory-update` ¶ | Stop | Never blocks. At ≥`HARNESS_CONTEXT_STEWARD_PCT` (default 75; `0` mutes the handoff) the detached update also writes `handoffs/YYYY-MM-DD-session-<id8>.md`, once per crossing; a failed spawn prints a NOT-saved note naming `claude-code memory update`; 3 spawns in all |
| `format-stop` | Stop | Formats this session's still-dirty edited files under its cwd (≤50, newest first): repo `make format\|fmt` per file, else the pyproject chain, else gofmt/ruff/prettier/shfmt; a missing formatter is one note per tool per session, never a failure |
| `review-simplicity` | PostToolUse (Write\|Edit) | Advisory: feeds back ≤8 comment, complexity and smell findings the edit introduced |
| `memory-start` ¶ | SessionStart | Injects the bank's focus, a `memory search` pointer and a `plans open:` line (≤5 plans under 7 days old with open tasks); repairs `projects/<slug>/memory`; on `clear`/`compact` adds the compaction record and handoffs (9,500-character cap) |
| `paths-resolve` | UserPromptSubmit | Reports existing paths in the prompt; translates Windows ⇄ `/mnt/…` on WSL |
| `memory-recall` | UserPromptSubmit | Injects up to 2 bank bodies plus 3 pointers within 900 chars; skipped under 4 prompt terms; a hit must match 2-3 of them; logs hits to `state/harness/memory/access/<name>.log` |
| `codex-agent-route` · `codex-routing-result` · `codex-routing-start` · `codex-subagent-start` | codex agent events | Codex-only routing hooks; never in Claude settings |
| `memory-compact` ¶ | PreCompact | Never blocks: updates memory for uncovered edits, waits ≤75 s for a fresh steward handoff (budget 90 s = settings timeout), writes a fact-only `handoffs/YYYY-MM-DD-compact-<id8>.md`; failures are stderr lines |
| `memory-end` | SessionEnd | After enough edits spawns `claude-code memory update`; the detached `memory watch` (`memory-bank.md`) |
| `secrets-out` ¶ | PostToolUse (Bash) | Claude only, never blocks: masks vault credential values in stdout/stderr at the same length (`updatedToolOutput`), so model and transcript see the mask; output under 12 bytes skips the vault; 2 s budget |
| `secrets-end` ¶ | SessionEnd | Claude only: a detached job masks the ended session's transcript, `subagents/` and `tool-results/` files in place at the same size once unmodified 30 s |
| `worktree-create` / `worktree-remove` | WorktreeCreate / WorktreeRemove | Create at `~/worktrees/<repo>/<name>` (honors `worktree.baseRef`, seeds `.venv`/`.env`, opens ledger `memory/worktrees/<name>.md`); remove refuses a dirty tree and `branch -d`s only a merged branch |
| `worktree-busy` ¶ | UserPromptSubmit | Applies vault env `HARNESS_WORKTREE` (`worktree-git.md` → Worktrees); drains this session's notices (≤5), reports each finished post-merge audit once (`audit of <sha> passed\|FAILED`) and shared-checkout divergence |
| stuck-leaf watchdog ¶ | within `worktree-busy` and `docs-plan-reminder`, main Claude session only | ≤1/min, one line per stale job and reason: dead while its record runs, past `HARNESS_WATCH_AGE` (10m, scaled by `Tier:`) and silent past `HARNESS_WATCH_SILENCE` (5m), running after its relay finished, or finished 5m uncollected; a dead job with a session id is resumed once, never restarted or cancelled; vault env `HARNESS_WATCH=off\|false\|0` disables, `dry` only names the resume |
| `worktree-claim` ¶ | PostToolUse (Write\|Edit\|MultiEdit; Bash async) | Never blocks: appends written or staged paths still dirty to `state/harness/hooks/claims/<pid>-<session>.jsonl`; synchronous on Codex, Copilot and Gemini (no async entry; about 10 ms per call) |
| `worktree-claim-check` ¶ | PreToolUse (Write\|Edit\|MultiEdit\|Bash) | Notes an edit of a dirty file another live session or sibling leaf claims; **asks** on staging or committing it, in a leaf under `~/.claude` the block becomes allow + note for Bash (Write/Edit stay blocked unless `--permissions allow`); `HARNESS_WORKTREE=lanes` extends it to tracked-file edits in the live checkout (`worktree-git.md` → Worktrees); infra errors allow with a warning |

### safety-scan main-session rules

A leaf is a subagent or a dispatched agent; rules match commands, never text, and are stated before `agentBashAllow`
turns a leaf's block or ask into allow. On codex, copilot, gemini and opencode an ask follows the dispatch's
`--allow`/`--deny` or blocks, naming the routes (a Claude session or your own terminal). A main-thread settings allow
match turns these rules off.

- `claude-code … --secret` (prints a credential) on any verb: main session only; a leaf gets credentials at launch and
  reports the vault name it needs. `[--host|--remote h] vault get --key --secret` (the master key) asks in main too;
  redacted forms (no `--secret`) are open to all.
- `kubectl|oc exec|delete` (`oc rsh`) on production (detection: hook-details → safety-scan): asks in main, refused in a
  leaf.
- `make|gmake audit|install|fmt|build|verify|bootstrap|adopt` and a fixing `lint-<kind>` in the harness tree (install
  targets also in its linked worktrees): refused in a leaf (run `claude-code agents diff --check --path <files>` or
  `claude-code agents fix --path <files>`); `make lint` passes.
- A leaf's `HARNESS_DISPATCH_*` unset, `env -u` or assignment in text that reaches a shell, `eval`, `source`, an
  interpreter or `ssh` (its string, a here-string, a heredoc no pure reader takes, a pipe into it, a script it runs):
  a nested dispatch inherits these limits.

## Authoring a gate

Prose is the instruction; the hook is the guarantee. Every gate:

1. **Triggers on a checkable condition** — transcript facts or file state, never self-report.
2. **Blocks with instructions and the evidence contract** — what artifact proves compliance.
3. **Releases only on evidence** — mtimes, receipts, exit codes.
4. **Caps and fails open** — a Stop gate that can trap a session needs a cap and a visible fail-open; a deny-one-call
   gate (`docs-placement`) rejects the input and the model retries. Infra errors fail open, except workflow stage
   route-preview errors; a `Blocks` hook that panics, times out (0.5 s before the host's) or gets an empty or
   unparseable payload blocks.

Test the block path, the evidence-release path and the fail-open cap. Probe a live guard with a script reading payloads
from a data file (quoted payloads in the probe command trip the guard) and tally allow, ask and block. Permission
guardrails live in `settings.json`'s `permissions` block (`internal/settings/template.json`; `ask` beats `allow`).
