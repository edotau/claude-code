---
paths:
  - "internal/hooks/**"
  - "internal/settings/hookspec.go"
---
# Hooks System

Hooks run as `$HOME/.claude/bin/.hookd <Event> [--<module>|--all|--async]` (the older `hook <Event>` form still works).
`make compile` builds this hooks-only binary beside `claude-code` with one shared stamp in `bin/.build-stamp`.
Non-hook verbs or a stamp mismatch after a partial install hand over to `bin/claude-code`, preserving behavior.
The safety scanner treats `.hookd` exactly like `claude-code`.

**`internal/settings/hookspec.go` is the only place a hook is declared** (one `Spec` per invocation).
Handlers live in `internal/hooks/`. To add, remove or retime
one, edit both and run `make install` for projections; an existing Claude `settings.json` needs a hand edit
(`settings-management.md`). `claude-code agents sync` re-renders each agent's native hook and config files;
`agents sync --check` exits 1 when one is stale.

## Groups and the dispatcher

`settings.Registry` lists the groups in run order; `codex` is agent-only, never in Claude settings.

| Group | Does |
| --- | --- |
| `safety` | Bash safety gate (block / ask / warn tiers) |
| `rtk` | Routes noisy command output through rtk (Claude, codex, opencode); Claude rewrites in place in every mode, auto included |
| `docs` | Markdown placement; plan template, dated plan copies, plan-task ticks |
| `dispatch` | Concurrency cap, subagent model fill, memory recall on spawn and every `agents ask` |
| `paths` | Resolves Windows ⇄ `/mnt/…` paths in prompts |
| `memory` | Focus and recall at start/prompt; steward handoff and update at Stop/SessionEnd/PreCompact |
| `format` | Formats this session's still-dirty edited files |
| `review` | Advisory `review-simplicity` finding after each edit; R28 `review-handback` and `review-decide` |
| `worktree` | Creates/removes under `~/worktrees/<repo>/`; ledger, claims, notices |
| `lane` | Merge on completion: a finished leaf's lane merges detached, or waits in the merge queue |
| `codex` | Codex only: child-agent routing and its log |

- **One synchronous entry per event** (`hook <Event>`, the union of its groups' matchers) reads the payload once, runs
  each group within its budget, recovers panics and merges results. `--<module>` runs one module; `--all` runs every
  module on the event except agent-only groups, applying agent applicability.
- **`safety` has its own process** (`hook PreToolUse --safety-scan`), so a hung group never delays or skips a block.
- **Async entry** (`hook PostToolUse --async`) runs the Bash side of `docs-plan-tick` and `worktree-claim`: the tool
  returns at once and the note reaches the model a turn later. A `settings.json` without that entry keeps them
  synchronous; a headless `claude -p` that exits first kills the run.
- **No per-group switch**: every declared group runs. Dev override, one launch only: `HARNESS_HOOK_SKIP=<module>[,…]`
  or `all` in the hook's own env (`all` spares `safety-scan` unless named); output notes each
  skip. Fix a misfiring module narrowly; delete an unwanted one (spec and handler), never skip it from the vault.

**Telemetry**: each module run appends one row to `state/harness/hooks/telemetry-YYYYMMDD.jsonl` (14 days, capped):
event, module, outcome, ms, 8-char session id, tool, and when a decision fired a `rule` and a `verdict` (`block`,
`ask`, `rewrite`, `held`, `refused`, `passed`); never command text, a path, a brief or a credential.
A day file at its 16 MiB cap stops recording and leaves `telemetry-YYYYMMDD.dropped`; later rows that day are missing.
`claude-code doctor` reports seven days of fires, denies, budget overruns and p95 duration per module.

## Applicability by agent

`settings.SpecsFor(agent, surface)` answers "which hooks run where". Surfaces: `native` (the agent's own hook file:
Claude settings, `~/.codex/hooks.json`, `~/.copilot/hooks`, `~/.gemini/config/hooks.json`), `hosted` (the harness
runs the module around a headless `opencode run`), `plugin` (opencode in-process). Codex, copilot and agy have no ask
decision, so an ask denies there; their other hooks are advisory unless noted.

| Agent · surface | Modules | Mode |
| --- | --- | --- |
| claude · native | every group that isn't agent-only; each harness launch's `--settings` adds `web` and `safety-claude-edit` | **block** (`safety-scan` asks on an interpreter script that may write a protected path; a subagent's `CLAUDE.md` write blocks); post/prompt hooks inject context |
| codex · native | `safety-scan`, `docs-placement`, `worktree-claim-check`, `paths-resolve`, `memory-recall`, `rtk-rewrite`, `codex-*`; PostToolUse `worktree-claim` (`apply_patch` too); SessionStart `memory-start`; Stop `memory-update`; SessionEnd `memory-end` (3s cap) | **block** |
| copilot · native | `safety-scan`, `docs-placement`, `worktree-claim-check`, `paths-resolve`, `memory-recall`; PostToolUse `worktree-claim`; sessionStart `memory-start`; Stop `memory-update`, `format-stop`; SessionEnd `memory-end` | PreToolUse **block** |
| gemini (agy) · native | `safety-scan`, `docs-placement`, `worktree-claim-check` (no `rtk-rewrite`: agy judges its own rules on the rewritten text); PostToolUse `docs-plan-mirror`, `docs-plan-tick`, `review-simplicity`, `worktree-claim`; Stop `docs-plan-reminder`, `memory-update`, `format-stop`; SessionStart `memory-start` | PreToolUse **block**; SessionStart injects a system step; Stop block = `continue`; PostToolUse output is `{}`, so its notes never reach the model |
| opencode · hosted | `Hosted` specs (docs, paths, memory, format, review; `safety-scan` and `docs-placement` are declared hosted but fire only through the plugin; no worktree notices) | **advisory**; `--pure` loads no plugin, so it is refused while a PreToolUse gate is bound; Stop and SessionEnd run only after a watched tool call |
| opencode · plugin | `safety-scan`, `docs-placement`, `worktree-claim-check`; then `rtk-rewrite` once the guards allow; `worktree-claim` in `tool.execute.after` | **block** (throws in `tool.execute.before`; the host then skips these); the rewrite sets `args.command`; the claim is a detached, non-blocking spawn per edit |

**Pi · plugin** (`pi`): only `safety-scan`, run by the guard extension, which registers the only
tools Pi has and sends each raw call to `claude-code hook PreToolUse --pi-guard`. That entry ignores every
`HARNESS_HOOK_*` variable (a skip, a changed agent), normalizes the path as Pi does, builds a Claude-shaped payload
from canonical fields only with `"harness_agent":"pi"`, and runs the module. Pi has no ask, so an ask is a block. Reply:
one JSON line; only `{"allow":true,"path":...}` lets the tool run (on
that path), `{"block":"<reason>"}` or anything else blocks. `docs-placement` and `worktree-claim-check` are not on
Pi's list (plan NEXT); `safety-scan` stays Bash-only, as for codex and gemini.
Pi itself has no permission prompts and no sandbox: it runs with `--no-builtin-tools`, and the guard's seven tools are
the only ones it has. Only calls of those seven are judged; a tool a package registers is blocked (the guard compares
real paths, so a run dir reached through a symlink still matches its own tools). The guard fails
closed: one that fails to load leaves Pi no tools, it refuses any Pi but the pinned one, and each dispatch checks a
load nonce. `safety-scan` judges bash. Pi tool to hook tool name:

| Pi tool | `bash` | `edit` | `write` | `read` | `grep` | `find` | `ls` |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Hook tool name | `Bash` | `Edit` | `Write` | `Read` | `Grep` | `Glob` | `Read` |

The path is normalized as Pi would: a leading `@`, `~` and Unicode spaces; `file://` and other forms the harness
cannot reproduce are blocked. Both the typed and the on-disk spelling are judged. Full description:
`/Users/edotau/.claude/rules/harness/agent.md` (Pi bullets).

## Event → hook matrix

Rows marked ¶ have full behavior in `skills/subagent-workflows/references/hook-details.md` under the hook's heading.

`safety-scan` has four rules and applies them to every agent, leaves included. Rules 1 to 3 check
each Bash call in order:

1. Attribution: an AI-attribution footer (`Co-Authored-By: Claude`, `Generated with Claude Code` and the like) in a
   commit, merge or tag message, or in a PR title or body, blocks (`CLAUDE.md` hard rule 3). The PR forms are
   `gh pr create` and `gh pr edit` (`--title`/`-t`, `--body`/`-b`, the contents of a `--body-file`/`-F` file) and
   `claude-code worktree pr create`; a message file the same command writes counts.
2. Deny rules: a command that matches a `permissions.deny` rule in `~/.claude/settings.json` (template fallback, no
   other file) blocks; `sudo` follows `Bash(sudo:*)`. A write to a path an `Edit(...)` deny glob covers blocks too,
   whether by redirect, `cp`/`mv`/`rm` or an interpreter script (`//` absolute, `~/`, `/` = `~/.claude/`, `**/`,
   relative = cwd); an interpreter script that only may write one asks.
3. CLAUDE.md: main session only (owner, 2026-11-24). A subagent (payload `agent_id`, or `HARNESS_DISPATCH_ID` set)
   that writes, moves, links or removes a file named `CLAUDE.md` (any directory, or a symlink to one; case-folded
   where the filesystem folds case) is blocked and told to hand the change back to the main session with the exact
   path and edit. A user-started Pi has no dispatch id, so it is the main session. Shapes: edit tools, `apply_patch`
   (tool or shell), redirect (a `$NAME` target the command assigns the file), `tee`, `sed -i`, `perl -i`,
   `cp`/`mv`/`install` (`-t DIR`), `ln` (source or link), `rm` and `git checkout|restore|rm|mv|clean`. Claude's edit
   tools reach the rule through every harness launch's `--settings` layer (group `safety-claude-edit`).
4. Lint suppression (owner, 2026-11-24; `internal/hooks/safety/suppress.go`): main session: ask; agents: deny. An
   Edit, Write, MultiEdit, NotebookEdit or `apply_patch` that ADDS a line with a lint or type-check suppression marker
   (`noqa`, `type: ignore`, `pylint: disable`, `pyright: ignore`, `eslint-disable`, `nolint`, `@ts-ignore`, `shellcheck
   disable`, Rust `allow`; one regexp table) is refused; lines already in the old text or file never count, and Bash
   and `.md`/`.mdx` files are exempt. The main session's ask is a rare exception: it leads "refactor is the default;
   approve only if the fix is disproportionate", then marker, file:line, the line and its reason (the comment after
   the marker or the comment line above, else "no reason given"). An agent gets a deny telling it to hand the line back
   in `UNFIXED` with its reason. Leaves reach it through `safety-claude-edit`; the main session needs the owner's
   `settings.json` `safety-scan` matcher to name the edit tools (`Bash|Write|Edit|MultiEdit|NotebookEdit`).

Claude asks; an agent without ask gets a block. A leaf gets no exception from any rule.
Commands are matched as invocations, not as data text. Blocks name the allowed route and the owner's `!`;
`claude-code doctor --audit` logs no command text.

| Hook | Event | What it does |
| --- | --- | --- |
| `safety-scan` ¶ | PreToolUse (Bash; Claude Write\|Edit\|MultiEdit via `--settings`) | Block a footer or a deny-rule match; ask on an interpreter script that may write a protected path; block a subagent's `CLAUDE.md` write (main session only); an edit adding a lint suppression asks in the main session, denies in agents |
| `rtk-rewrite` ¶ | PreToolUse (Bash) | `rtk rewrite`s a command; Claude in place in every mode (auto included) with no permission decision, so auto mode's classifier still judges it; codex (as `allow` + `updatedInput`) and opencode via `rtk hook <agent>`, never under Pi code mode, only when every Bash PreToolUse gate the agent runs allows both forms; vetoes, any rtk failure or timeout, and (codex, opencode) env-altered commands pass (fails open, not a `Blocks` gate); exact bytes: `rtk bypass <cmd>` |
| `bash-timeout` | PreToolUse (Bash; Claude only) | Sets the Bash `timeout` of a foreground `agents ask`, `agents wait` or `agents result -f` to its `--timeout` + 10 s (600000 ms when none, and the host ceiling); never blocks, asks or lowers a larger timeout; a `wait --timeout` over 590 s keeps the 600000 ms cap, so shorten it |
| `docs-placement` ¶ | PreToolUse (Write\|Edit\|MultiEdit\|Bash) | **Block** a new `.md` at a repo root or in a gitignored `docs/`, naming the home (`docs-placement.md`); a leaf's Bash block becomes allow + note; redirects a new `*plan*.md` to the dated plans home; a first plan write needs the `docs/templates/plan.md` headings |
| `dispatch-effort` ¶ | PreToolUse (Agent\|Task\|Workflow) | Gates the type (`coworker:<agent>` → `<agent>`), pins model and effort by `agent.TaskTier` for an unpinned type, claims slots, labels, adds the recall block to every brief and dispatched `agents ask` (`withRecall`), all in one `updatedInput` |
| dispatch cap | within `dispatch-effort` | **24 in flight per available account** (a profile's `concurrency` overrides), machine-wide `24 × accounts` clamped to **4 × CPUs**; a blocked dispatch claims nothing; infra errors fail open; `doctor` shows `in-flight`. Description `<model>: <task>`, the model the run resolves (`claude-code agents label <type>`); a wrong label is rewritten, never denied; an auto ask's label is a peek |
| `dispatch-failure` · `dispatch-workflow` · `dispatch-stop` ¶ | PostToolUseFailure · PostToolUse (Workflow\|Bash) · SubagentStop | Retire the spawn's slots; `dispatch-stop` holds a leaf's stop while its job runs (≤2 h); for an external-type leaf with no receipt, attaches finished jobs or passes with a relay note; saves a held report |
| `dispatch-handback` ¶ | PreToolUse (SubagentHandback\|Bash) | `dispatch-stop`'s leaf gates on the hand-back: waits ≤13 s for a running job, attaches each finished result with its receipt, else holds naming `claude-code agents result -f <id>`; on Bash, only in a subagent of an external type (never a `claude` leaf or the main session), refuses duplicate polls and limits its commands; on a hand-back naming a plan task (dispatch `plan_task` or the brief's `Task:`), runs its allowlisted `verify:` plus the scoped gate on its owned dirty files within the budget and puts `verify <id>: …` lines on top of the message (never holds it) |
| `docs-plan-mirror` | PostToolUse (Write\|Edit) | Mirrors the undated draft to a dated, repo-tagged file from the first write (slug: `Suggested slug:`, else the H1), under a shared lock |
| `docs-plan-save` | PostToolUse (ExitPlanMode) | Files approved text at the mirror destination, adds a missing checklist, verifies, removes the draft |
| `docs-plan-tick` / `docs-plan-reminder` ¶ | PostToolUse (Bash, async) / Stop | The main session's verified commit on main starts one detached `plan tick --verify` per plan for the tasks its `Task:` trailers name; it ticks each whose `verify:` and scoped gate pass on that sha in a scratch checkout and posts a notice; red stays open with the failing command and tail (coverage never ticks), noting `owns:` files left out (trailer, main and lane rules: `worktree-git.md` rule 3); `docs-plan-reminder` only hosts the Stop watchdog |
| `memory-update` ¶ | Stop | Never blocks. At ≥`HARNESS_CONTEXT_STEWARD_PCT` (default 75; `0` mutes the handoff) the detached update also writes `handoffs/YYYY-MM-DD-session-<id8>.md`, once per crossing; a failed spawn prints a NOT-saved note naming `claude-code memory update`; 3 spawns in all |
| `format-stop` | Stop | Formats this session's still-dirty edited files under its cwd (≤50, newest first): repo `make format\|fmt` per file, else the pyproject chain, else gofmt/ruff/prettier/shfmt; a missing formatter is one note per tool per session, never a failure |
| `review-simplicity` | PostToolUse (Write\|Edit) | Advisory: feeds back ≤8 comment, complexity and smell findings the edit introduced |
| `review-handback` (and `review-notified`, on a task-notification prompt) | PostToolUse (Agent\|Task\|TaskOutput) | Never blocks: parses only the last `UNFIXED:` line of a leaf report: a list adds `Fix these now` context with the items, `none` adds nothing, no line adds a read-it note (R28) |
| `review-decide` | Stop | Main session only: blocks once (not when `stop_hook_active`) on an unanswered AskUserQuestion mentioning fix, finding or review, or a fixed ask such as "want me to fix" outside quotes (R28) |
| `memory-start` ¶ | SessionStart | Injects the bank's focus, a `memory search` pointer and a `plans open:` line (≤5 plans under 7 days old with open tasks); repairs `projects/<slug>/memory`; on `clear`/`compact` adds the compaction record and handoffs (9,500-character cap) |
| `paths-resolve` | UserPromptSubmit | Reports existing paths in the prompt; translates Windows ⇄ `/mnt/…` on WSL |
| `memory-recall` | UserPromptSubmit | Injects up to 2 bank bodies plus 3 pointers within 900 chars; skipped under 4 prompt terms; a hit must match 2-3 of them; logs hits to `state/harness/memory/access/<name>.log` |
| `codex-agent-route` · `codex-routing-result` · `codex-routing-start` · `codex-subagent-start` | codex agent events | Codex-only routing hooks; never in Claude settings |
| `memory-compact` ¶ | PreCompact | Never blocks: updates memory for uncovered edits, waits ≤75 s for a fresh steward handoff (budget 90 s = settings timeout), writes a fact-only `handoffs/YYYY-MM-DD-compact-<id8>.md`; failures are stderr lines |
| `memory-end` | SessionEnd | After enough edits spawns `claude-code memory update`; the detached `memory watch` (`memory-bank.md`) |
| `secrets-out` ¶ | PostToolUse (Bash) | Claude only, never blocks: masks vault credential values in stdout/stderr at the same length (`updatedToolOutput`), so model and transcript see the mask; output under 12 bytes skips the vault; 2 s budget |
| `secrets-end` ¶ | SessionEnd | Claude only: a detached job masks the ended session's transcript, `subagents/` and `tool-results/` files in place at the same size once unmodified 30 s |
| `web-tavily` | PostToolUse (WebFetch\|WebSearch) | Claude only, from every launch's `--settings` (`LaunchSettings`), never `settings.json`; never blocks: replaces the result with Tavily's extract or search (`allowed_domains`/`blocked_domains` kept) in the tool's own shape; skips a search on a Databricks profile (the proxy searched via Tavily); no `TAVILY_API_KEY` is a no-op, any error keeps the built-in result; 25 s |
| `worktree-create` / `worktree-remove` | WorktreeCreate / WorktreeRemove | Create at `~/worktrees/<repo>/<name>` (honors `worktree.baseRef`, seeds `.venv`/`.env`, opens ledger `memory/worktrees/<name>.md`); remove refuses a dirty tree and `branch -d`s only a merged branch |
| `worktree-clean-end` | SessionEnd | Never blocks: removes the repo's merged, clean, idle lanes that did work (cherry-picked, or a branch with its own commits; fresh lanes stay), one stderr line each |
| `worktree-busy` ¶ | UserPromptSubmit | Applies vault env `HARNESS_WORKTREE` (`worktree-git.md` → Worktrees); drains this session's notices (≤5), reports each finished post-merge audit once (`audit of <sha> passed\|FAILED`) and shared-checkout divergence |
| stuck-leaf watchdog ¶ | within `worktree-busy` and `docs-plan-reminder`, main Claude session only | ≤1/min, one line per stale job and reason: dead while its record runs, past `HARNESS_WATCH_AGE` (10m, scaled by `Tier:`) and silent past `HARNESS_WATCH_SILENCE` (5m), running after its relay finished, or finished 5m uncollected; a dead job with a session id is resumed once, never restarted or cancelled; while healthy jobs run, one `running: Task <id>\|Job <id> (agent, age); …` line (≤4, ≤200 chars); vault env `HARNESS_WATCH=off\|false\|0` disables, `dry` only names the resume |
| `worktree-claim` ¶ | PostToolUse (Write\|Edit\|MultiEdit; Bash async) | Never blocks: appends written or staged paths still dirty to `state/harness/hooks/claims/<pid>-<session>.jsonl`; synchronous on Codex, Copilot and Gemini (no async entry; about 10 ms per call) |
| `worktree-claim-check` ¶ | PreToolUse (Write\|Edit\|MultiEdit\|Bash) | Notes an edit of a dirty file another live session or sibling leaf claims; **asks** on staging or committing it, in a leaf under `~/.claude` the block becomes allow + note for Bash; `HARNESS_WORKTREE=lanes` extends it to tracked-file edits in the live checkout (`worktree-git.md` → Worktrees); infra errors allow with a warning |
| `lane-merge` | SubagentStop · UserPromptSubmit | Never blocks or asks (R27). A stopped leaf's lane (its `meta.json` `worktreePath`, else the one lane its claims sit in; a background `agents ask` job that wrote files in a lane starts the same worker at its end) goes to a detached worker, which waits for the leaf to finish and the job to exit: ahead, clean, `UNFIXED: none`, not busy and touching no `internal/hooks/safety/`, `hookspec.go` or `settings*.json` → `worktree merge <lane> --resolve-agent pi`, serialized across lanes, ≤3 tries on a moved target, then a `notice (lane-merge)`; anything else (dirty, unfixed, no UNFIXED line, busy, restriction, conflict, gate) is one entry in `state/harness/worktree/merge-queue.json`, shown as a `merge queue:` line with its next command on every prompt (≤5) and by `worktree list` until a merge or the lane's removal drops it |

**`dispatch-effort` and a spawn mod.** A mod that rewrites at `agent.spawn` calls `hook PreToolUse --dispatch-effort`
with a top-level `harness_spawn` in the payload: `"ready"` plus `session_id` writes the session's marker
(`state/harness/hooks/spawn-mod-ready/<session>`, keyed to the session's process, ignored after 7 days) and prints
`{}`; `"query"` plus the Agent fields and `transcript_path` prints one object of the changed `description`, `prompt`,
`subagentType` and `model`, claiming nothing (no recall).
With the marker, PreToolUse skips the pin and label (the mod's query computes them once)
and emits only the recall note; denials are unchanged, and a `coworker:<agent>` retype stays a hook
`updatedInput` (the host validates the type before `agent.spawn`). The mod is `mods/dispatch-spawn`
(`claude plugin test mods/dispatch-spawn`), loaded with `slash-usage` by `CLAUDE_CODE_PLUGIN_DIRS`
(`internal/assets/files/settings/template.json`; `doctor` row `mods` flags a
settings.json without it); without it (other CLIs, `claude -p`) no marker exists and the hook rewrites
as before.

## Authoring a gate

Prose is the instruction; the hook is the guarantee. Every gate:

1. **Triggers on a checkable condition** — transcript facts or file state, never self-report.
2. **Blocks with instructions and the evidence contract** — what artifact proves compliance.
3. **Releases only on evidence** — mtimes, receipts, exit codes.
4. **Caps and fails open** — a Stop gate that can trap a session needs a cap and a visible fail-open; a deny-one-call
   gate (`docs-placement`) rejects the input and the model retries. Infra errors fail open, except workflow stage
   route-preview errors; a `Blocks` hook that panics, times out (0.5 s before the host's) or gets an empty or
   unparsable payload blocks.

Test the block path, the evidence-release path and the fail-open cap. Probe a live guard with a script reading payloads
from a data file (quoted payloads in the probe command trip the guard) and tally allow, ask and block. Permission
guardrails live in `settings.json`'s `permissions` block (`internal/assets/files/settings/template.json`; `ask` beats
`allow`).

## Hook performance and contract

Rules for every new or changed hook. Measure with `claude-code doctor` (p95 per module) before and after.

1. **Loosen only** — a hook never adds a permission error or restriction; allowing more, or new non-blocking output, is
   fine. A new `Blocks` hook needs the owner's yes (R13). A blocked leaf is flag-first: a hook loosens only for a
   recurring false positive (`skills/subagent-workflows/SKILL.md` → Error handling).
2. **Auto-fix first** — correct a deterministic problem (`updatedInput`, a fixer, a CLI apply); deny or exit 2 only when
   code cannot decide.
3. **Narrowest matcher** — `hookspec.go` `Matcher` is the filter; `*` spawns a process per event.
4. **Rarest event** — a per-edit or per-prompt hook runs thousands of times a day; SessionStart and SessionEnd run once.
5. **Budget and cap** — every Spec sets `Budget` (default 5 s); a module that waits on I/O takes
   `core.HookContext(name)` and caps each wait below it, degrading, never blocking.
6. **Per-prompt cost** — UserPromptSubmit modules run concurrently, so the slowest sets the wait; move slow work to a
   detached job whose note lands next turn.
7. **Cache by content key** — a result that depends only on a file, commit or prompt hash is cached under
   `state/harness/` with that key; never cache a decision across sessions.
8. **Independent modules** — handlers run concurrently and merge in declaration order; one never reads another's
   side effects. Order a dependency inside one module.
9. **Stdout is the result, stderr the log** — `core.Result` is the only stdout writer; a stray print breaks the host's
   parse. A block is exit 2 with the reason on stderr (agy: a JSON deny).
10. **Idempotent** — a hook may run twice (retry, resume, parallel session); appends are keyed by session id.
11. **No secrets in reach** — read a credential through `vault` verbs, never the inherited env; never log command text,
    a path, a brief or a credential.
12. **Fail open on infra, closed only for `Blocks`** — see "Authoring a gate" item 4.

Debugging a slow hook:

- `claude-code doctor` ranks modules by p95 and overruns; raw rows: `state/harness/hooks/telemetry-YYYYMMDD.jsonl`.
- Replay one module with a saved payload: `claude-code hook PreToolUse --safety-scan < payload.json`; read the exit
  code and stdout separately (`set -o pipefail`).
- Run a suspect module off for one launch with `HARNESS_HOOK_SKIP=<module>`; fix or delete it, never leave it skipped.
- A `.dropped` marker beside a day file means rows after the cap are missing: do not read that tail as quiet.
