---
paths:
  - "internal/hooks/**"
  - "internal/settings/hookspec.go"
---
# Hooks System

Hooks run as `claude-code hook <Event> [--<module>|--all|--async]`. **`internal/settings/hookspec.go` is the only
place a hook is declared** (one `Spec` per invocation); the handler lives in `internal/hooks/`. Add, remove or retime:
edit both, then `make install` for projections; an existing Claude `settings.json` needs a hand edit
(`settings-management.md`). `claude-code agents sync` also re-renders each agent's native hook and config files;
`agents sync --check` exits 1 when one is stale.

## Groups and the dispatcher

`settings.Registry` lists groups in run order: `safety`, `permission`, `rtk`, `docs`, `dispatch`, `paths`, `memory`,
`format`, `review`, `worktree`, `codex`; `permission` and `codex` are agent-only, never in Claude settings. Settings
carry one synchronous dispatcher entry per event (`hook <Event>`) matching the union of its groups' matchers, plus a
background `hook PostToolUse --async` entry for the Bash alternatives of `docs-plan-tick` and `worktree-claim`; the
dispatcher reads the payload once, runs each group within its budget, recovers panics and merges results.
`--<module>` runs one module; `--all` runs every module on the event, skipping agent-only groups and applying each
module's agent applicability.

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
| codex · native | `safety-scan`, `permission-gate`, `docs-placement`, `paths-resolve`, `memory-recall`, `codex-*`; Stop `memory-update`; SessionEnd `memory-end` (3s cap) | **block**; `memory-end` advisory |
| copilot · native | `safety-scan`, `docs-placement`, `paths-resolve`, `memory-recall`; Stop `memory-update`, `format-stop`; SessionEnd `memory-end` | PreToolUse **block**; the rest advisory |
| gemini (agy) · native | `safety-scan`, `permission-gate` | **block** |
| opencode · hosted | `Hosted` specs (docs, paths, memory, format, review; `safety-scan` and `docs-placement` are declared hosted but fire only through the plugin; no worktree notices) | **advisory** (`--pure` loads no plugin, so it is refused while a PreToolUse gate is bound); Stop and SessionEnd run only after a watched tool call |
| opencode · plugin | `safety-scan`, `permission-gate`, `docs-placement`, `worktree-claim-check` | **block** (throws in `tool.execute.before`; the host then skips these) |

`permission-gate` judges each call with `Permissions.Decide` against `HARNESS_DISPATCH_PERMISSIONS`; how each agent
reaches it, and the gated Claude launches: `agent.md` → Bash deny via `permission-gate`.

## Event → hook matrix

Rows marked ¶ have full behavior text in `skills/subagent-workflows/references/hook-details.md`.

| Hook | Event | What it does |
| --- | --- | --- |
| `safety-scan` | PreToolUse (Bash) | **Block** destructive shapes (`rm -rf /`/`$HOME`, fork bomb, `curl | sh`, `base64 -d | sh`, a destructive `printf` piped to a shell), Bash writes to files the `Edit(...)` deny rules protect, and `commit --amend`/`rebase`/`reset <commit>` in a shared checkout; **ask** on Claude (block on codex/copilot) for `git stash/checkout/switch/reset/restore/clean` under `~/.claude`, AI-attribution commit footers and intent-dependent shapes (`sudo`, `DROP TABLE`, `rm -rf .git`, main/master force-push); inert text is ignored, any `<<` scans heredocs (a plain Python stdin script's body skips the ask-level shell words; a command written to a file and run in the same line is checked), an `apply_patch` call's patch text is not scanned; blocks log to `claude-code doctor --audit` (reason, pattern and session id, never the command). Full shape list: `skills/subagent-workflows/references/hook-details.md` |
| `permission-gate` | PreToolUse (Bash\|Write\|Edit\|Read\|WebFetch\|Glob\|Grep) | **Block** a call outside the dispatch's `Permissions` (5s); `apply_patch` is judged as an Edit per file; agent-only |
| `rtk-rewrite` ¶ | PreToolUse (Bash) | Rewrites a command via `rtk rewrite` (never sets a permission decision); Claude sessions only, skipped in auto mode; exact bytes: `rtk bypass <cmd>` |
| `docs-placement` | PreToolUse (Write\|Edit\|MultiEdit\|Bash) | Blocks a new `.md` at a repo root or under a gitignored `docs/`, naming the home (`docs-placement.md`); also checks `apply_patch` headers and Bash write targets; rewrites a new `*plan*.md` at a repo root or `docs/` to the dated plans home (denies with the fix where the host cannot rewrite); rewrites (with a note) a Claude first write to a native plan draft that lacks only `# Plan:` or `## Tasks`, and denies any other first write lacking the `docs/templates/plan.md` headings, listing them; `HARNESS_HOOK_SKIP=docs-placement` skips both |
| `dispatch-effort` ¶ | PreToolUse (Agent\|Task\|Workflow) | Gates type (rewrites `coworker:<agent>` to `<agent>` on every brief); pins model and effort by `agent.TaskTier` for an unpinned type; a `Run:` line's `--model auto` labels the model auto ranks; claims slots, recalls context (a context note in auto mode or on a `Run:` brief, else appended to the brief), labels the row; every fix rides one `updatedInput` with a note line each (a `Run:` prompt stays byte-identical); denies, naming the fix, a `Run:` line naming a missing workspace or (with `--fallback none`) an unserved model |
| dispatch cap | within `dispatch-effort` | **24 in flight per available account** (a profile's `concurrency` overrides), machine-wide `24 × accounts` clamped to **4 × CPUs**; a blocked dispatch claims nothing, infra errors fail open; `doctor` shows `in-flight`. The description starts `<model>: <task>`, the model the run resolves (`claude-code agents label <type> [--profile p] [--model m] --task FILE` prints it); a wrong or missing label is rewritten on every brief, never denied |
| `dispatch-failure` · `dispatch-workflow` · `dispatch-stop` ¶ | PostToolUseFailure · PostToolUse (Workflow) · SubagentStop | Retire the spawn's slots; `dispatch-stop` blocks a leaf's stop while its job still runs (≤2 h); a `codex`/`copilot`/`gemini`/`opencode` leaf without an `ask: ran on <agent> ·` line is never refused: held while its job runs, then passed with the record's receipt; a never-run `Run:` line (no file edits) is run by the hook; relay work passes with a note; a held report is saved to the reports home |
| `dispatch-handback` | PreToolUse (SubagentHandback) | Runs `dispatch-stop`'s two leaf gates on the hand-back call, which ends a background leaf before SubagentStop; the note, receipt and job result are prepended/appended to the hand-back `message`; it waits up to 13 s for a running job and passes at once when it ends, else holds; a never-run `Run:` line with no file edits is run by the hook, and relay work passes with a note; when a leaf started several jobs, every finished job's answer is attached in dispatch order, each under its own receipt line, including jobs that finished before the wait; only ids the leaf's own `agents ask\|dispatch\|result` call printed count, never ids from listings, logs, files or the answer body; a hold saves every finished answer and the later pass attaches them all; the hook's `Run:` launch is claimed with an exclusive create, so overlapping hand-backs start it once |
| `docs-plan-mirror` | PostToolUse (Write\|Edit) | Mirrors the undated draft to a dated, repo-tagged file from the first write (slug from `Suggested slug:`, else the H1); a shared lock serializes it |
| `docs-plan-save` | PostToolUse (ExitPlanMode) | Files approved text at the mirror destination, adds a missing checklist, verifies, removes the draft |
| `docs-plan-tick` / `docs-plan-reminder` ¶ | PostToolUse (Bash, async) / Stop | A landed commit ticks the plan tasks its `owns:` covers (`claude-code plan status`); partly landed tasks are reported once at Stop. The Bash work runs in the async entry (`hook PostToolUse --async`): the tool returns at once and the note reaches the model a turn later; a `settings.json` without that entry keeps it synchronous, and a headless `claude -p` that exits first kills it |
| `memory-update` ¶ | Stop | Context steward at ≥`HARNESS_CONTEXT_STEWARD_PCT` (default 75; `0` mutes the prompt), then the memory update; never blocks, but past the threshold its handoff prompt continues the turn once per crossing; a failed spawn prints a NOT-saved note naming `claude-code memory update` at once; each of the next two Stops retries the spawn once (3 spawns in all), a Stop whose retry succeeds is silent, and the note shows once more at the cap (SessionEnd/PreCompact retry too) |
| `format-stop` | Stop | Formats this session's still-dirty edited files in its cwd subtree (≤50, newest first): repo `make format\|fmt` with a per-file variable, else the pyproject chain, else gofmt/ruff/prettier/shfmt; a formatter that is not installed prints one note per tool per session, never a failure |
| `review-simplicity` | PostToolUse (Write\|Edit) | Advisory: feeds back ≤8 comment, complexity and smell findings the edit introduced |
| `memory-start` | SessionStart | Injects this repo's and the home bank's focus (Current Focus, Blockers, Next Steps, In Progress) plus a `memory search --all` pointer when `memory/index.md` exists; repairs `projects/<slug>/memory` (links to the store root); on `compact`/`clear` reports the compaction record and the newest handoff under 24 h (`memory-bank.md` → Who writes what) |
| `paths-resolve` | UserPromptSubmit | Reports existing paths in the prompt; translates Windows ⇄ `/mnt/…` on WSL |
| `memory-recall` | UserPromptSubmit | Injects up to 2 bank bodies plus 3 pointers within 900 chars; skipped under 4 prompt terms; a hit must match 2-3 of them; logs hits to `state/harness/memory/access/<key>.log` |
| `codex-agent-route` · `codex-routing-result` · `codex-routing-start` · `codex-subagent-start` | codex agent events | Codex-only routing hooks; never in Claude settings |
| `memory-compact` | PreCompact | Dispatches a background update for uncovered edits and leaves a per-session record under `state/harness/compact/`; never blocks |
| `memory-end` | SessionEnd | After enough edits spawns `claude-code memory update`; the detached `memory watch` (`memory-bank.md`) |
| `worktree-create` / `worktree-remove` | WorktreeCreate / WorktreeRemove | Create at `~/worktrees/<repo>/<name>` (honors `worktree.baseRef`, seeds `.venv`/`.env`, opens ledger `memory/worktrees/<key>--<name>.md`); remove refuses a dirty tree and `branch -d`s only a merged branch |
| `worktree-busy` ¶ | UserPromptSubmit | Vault env `HARNESS_WORKTREE` (`worktree-git.md` → Worktrees); drains this session's notices (≤5) and adds a divergence line in a diverged shared checkout |
| `worktree-claim` | PostToolUse (Write\|Edit\|MultiEdit; Bash async) | Appends written and staged paths still dirty after the call to `state/harness/hooks/claims/<pid>-<session>.jsonl`; each line records the owner's start token, so a file whose pid was reused is dropped; never blocks. Write\|Edit\|MultiEdit stays synchronous; Bash runs in the async entry, so a `settings.json` without it keeps Bash synchronous and a headless `claude -p` that exits first kills the run |
| `worktree-claim-check` ¶ | PreToolUse (Write\|Edit\|MultiEdit\|Bash) | Notes before an edit that a dirty file is claimed by another live session or sibling leaf; asks on a staging or commit command that includes one (`-p`, a dry run and `-N` pass), blocks in a leaf; infra errors allow with a warning |

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
