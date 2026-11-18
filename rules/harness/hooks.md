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
| claude · native | every group that isn't agent-only | **block** (`safety` asks only on destructive SQL and script writes); post/prompt hooks inject context |
| codex · native | `safety-scan`, `permission-gate`, `docs-placement`, `paths-resolve`, `memory-recall`, `codex-*`; Stop `memory-update`; SessionEnd `memory-end` (3s cap) | **block**; `memory-end` advisory |
| copilot · native | `safety-scan`, `docs-placement`, `paths-resolve`, `memory-recall`; Stop `memory-update`, `format-stop`; SessionEnd `memory-end` | PreToolUse **block**; the rest advisory |
| gemini (agy) · native | `safety-scan`, `permission-gate`, `docs-placement`, `worktree-claim-check`; PostToolUse `docs-plan-mirror`, `docs-plan-tick`, `review-simplicity`, `worktree-claim`; Stop `docs-plan-reminder`, `memory-update`, `format-stop`; SessionStart `memory-start` | PreToolUse **block** (an ask denies); SessionStart injects a system step; Stop block = `continue`; PostToolUse output is `{}`, so its notes never reach the model |
| opencode · hosted | `Hosted` specs (docs, paths, memory, format, review; `safety-scan` and `docs-placement` are declared hosted but fire only through the plugin; no worktree notices) | **advisory** (`--pure` loads no plugin, so it is refused while a PreToolUse gate is bound); Stop and SessionEnd run only after a watched tool call |
| opencode · plugin | `safety-scan`, `permission-gate`, `docs-placement`, `worktree-claim-check` | **block** (throws in `tool.execute.before`; the host then skips these) |

`permission-gate` judges each call with `Permissions.Decide` against `HARNESS_DISPATCH_PERMISSIONS`; how each agent
reaches it, and the gated Claude launches: `agent.md` → Bash deny via `permission-gate`.

## Event → hook matrix

Rows marked ¶ have full behavior text in `skills/subagent-workflows/references/hook-details.md`.

| Hook | Event | What it does |
| --- | --- | --- |
| `safety-scan` | PreToolUse (Bash) | **Block** destructive shapes (`rm -rf /`/`$HOME`, fork bomb, `curl | sh`, `base64 -d | sh`, a destructive `printf` piped to a shell), Bash writes to files the `Edit(...)` deny rules protect (a leaf, subagent or dispatched, can never write `settings.json` or `settings.local.json`, by Bash or an edit tool; a leaf also cannot set, unset or `env -u`-clear a `HARNESS_DISPATCH_*` variable, since a nested dispatch inherits its limits), `commit --amend`/`rebase`/`reset <commit>` and branch-ref moves or deletions that lose commits (`branch -f|-D|-M`, `update-ref`) in a shared checkout (a leaf counts as shared only under `~/.claude`, and the live-leaves trigger applies only there), a secret-reading command (`vault get`, `gh auth token`, `env`) piped into `curl`/`nc`/`wget`, and (each naming the allowed route and the owner's `!`) `sudo` in command position, `git stash/checkout/switch/reset/restore/clean` under `~/.claude` (a new branch, an unstage, `stash list`, `clean -n`, linked worktrees and temp dirs pass), AI-attribution commit footers, `rm -rf .git` and a main/master force-push; **ask** on Claude (block on codex/copilot/agy) only for destructive SQL and a script writing a protected file; those rules fire only on a real invocation, never on words in data (a brief to `claude-code agents`, a heredoc written to a file, a plain Python stdin script, a grep/sed pattern, echo text, a commit message); hard blocks still scan every heredoc body and skip only the words of `claude-code agents`, `git commit|log`, `grep`/`rg`/`ag`, `jq`, `test` and an `echo`/`printf` not fed to a runner; a command written to a file and run in the same line is checked, an `apply_patch` call's patch text is not scanned; blocks log to `claude-code doctor --audit` (reason, pattern and session id, never the command). Full shape list: `skills/subagent-workflows/references/hook-details.md` |
| `permission-gate` | PreToolUse (Bash\|Write\|Edit\|Read\|WebFetch\|Glob\|Grep) | **Block** a call outside the dispatch's `Permissions` (5s); `apply_patch` is judged as an Edit per file; agent-only |
| `rtk-rewrite` ¶ | PreToolUse (Bash) | Rewrites a command via `rtk rewrite` (never sets a permission decision); Claude sessions only, skipped in auto mode; exact bytes: `rtk bypass <cmd>` |
| `docs-placement` | PreToolUse (Write\|Edit\|MultiEdit\|Bash) | Blocks a new `.md` at a repo root or under a gitignored `docs/`, naming the home (`docs-placement.md`); also checks `apply_patch` headers and Bash write targets; rewrites a new `*plan*.md` at a repo root or `docs/` to the dated plans home (denies with the fix where the host cannot rewrite); rewrites (with a note) a Claude first write to a native plan draft that lacks only `# Plan:` or `## Tasks`, and denies any other first write lacking the `docs/templates/plan.md` headings, listing them; `HARNESS_HOOK_SKIP=docs-placement` skips both |
| `dispatch-effort` ¶ | PreToolUse (Agent\|Task\|Workflow) | Gates type (rewrites `coworker:<agent>` to `<agent>` on every brief); pins model and effort by `agent.TaskTier` for an unpinned type; a `Run:` line's `--model auto` labels the model auto ranks; claims slots, recalls context (a context note in auto mode or on a `Run:` brief, else appended to the brief), labels the row; every fix rides one `updatedInput` with a note line each (a `Run:` prompt stays byte-identical); denies, naming the fix, a `Run:` line naming a missing workspace or (with `--fallback none`) an unserved model |
| dispatch cap | within `dispatch-effort` | **24 in flight per available account** (a profile's `concurrency` overrides), machine-wide `24 × accounts` clamped to **4 × CPUs**; a blocked dispatch claims nothing, infra errors fail open; `doctor` shows `in-flight`. The description starts `<model>: <task>`, the model the run resolves (`claude-code agents label <type> [--profile p] [--model m] --task FILE` prints it); a wrong or missing label is rewritten on every brief, never denied |
| `dispatch-failure` · `dispatch-workflow` · `dispatch-stop` ¶ | PostToolUseFailure · PostToolUse (Workflow\|Bash) · SubagentStop | Retire the spawn's slots; `dispatch-stop` blocks a leaf's stop while its job still runs (≤2 h); a `codex`/`copilot`/`gemini`/`opencode` leaf without an `ask: ran on <agent> ·` line is never refused: held while its job runs, then passed with the record's receipt; a never-run `Run:` line (no file edits; memory reports, handoffs and notes do not count) is run by the hook; relay work passes with a note; a held report is saved to the reports home; on Bash, `dispatch-workflow` (the synchronous entry, so the line reaches the model) gives a relay whose own `agents result` poll returned on an ended job the context line `job <id> ended: hand back now with this result` |
| `dispatch-handback` | PreToolUse (SubagentHandback\|Bash) | Runs `dispatch-stop`'s two leaf gates on the hand-back call, which ends a background leaf before SubagentStop; the note, receipt and job result are prepended/appended to the hand-back `message`; it waits up to 13 s for a running job and passes at once when it ends, else holds; a never-run `Run:` line with no file edits (memory reports, handoffs and notes do not count) is run by the hook, and relay work passes with a note; when a leaf started several jobs, every finished job's answer is attached in dispatch order, each under its own receipt line, including jobs that finished before the wait; only ids the leaf's own `agents ask\|dispatch\|result` call printed count, never ids from listings, logs, files or the answer body, except one its own successful `agents result\|wait <id>` named whose record is this session's, started within 2 min after the leaf's ask, with that ask's agent and a prompt holding its brief; a hold saves every finished answer and the later pass attaches them all; the hook's `Run:` launch is claimed with an exclusive create, so overlapping hand-backs start it once; every hold names `claude-code agents result -f <id>` to run once (never a sleep); on Bash it refuses a relay's poll of a finished job it already handed back (`already reported: stop`) and a second follow poll of one job while the first is in flight or backgrounded with a live writer (`one poller only`) |
| `docs-plan-mirror` | PostToolUse (Write\|Edit) | Mirrors the undated draft to a dated, repo-tagged file from the first write (slug from `Suggested slug:`, else the H1); a shared lock serializes it |
| `docs-plan-save` | PostToolUse (ExitPlanMode) | Files approved text at the mirror destination, adds a missing checklist, verifies, removes the draft |
| `docs-plan-tick` / `docs-plan-reminder` ¶ | PostToolUse (Bash, async) / Stop | A landed commit ticks the plan tasks its `owns:` covers (`claude-code plan status`); partly landed tasks are reported once at Stop. The Bash work runs in the async entry (`hook PostToolUse --async`): the tool returns at once and the note reaches the model a turn later; a `settings.json` without that entry keeps it synchronous, and a headless `claude -p` that exits first kills it |
| `memory-update` ¶ | Stop | Context steward at ≥`HARNESS_CONTEXT_STEWARD_PCT` (default 75; `0` mutes the handoff) makes the one detached update also write `handoffs/YYYY-MM-DD-session-<id8>.md` (the hook picks the path, kept in the marker), once per crossing; pending/running jobs are rechecked under the stamp lock and count as covered; no prompt, no continuation (compaction is `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE`); then the memory update; never blocks; a failed spawn prints a NOT-saved note naming `claude-code memory update` at once; each of the next two Stops retries the spawn once (3 spawns in all), a Stop whose retry succeeds is silent, and the note shows once more at the cap (SessionEnd/PreCompact retry too) |
| `format-stop` | Stop | Formats this session's still-dirty edited files in its cwd subtree (≤50, newest first): repo `make format\|fmt` with a per-file variable, else the pyproject chain, else gofmt/ruff/prettier/shfmt; a formatter that is not installed prints one note per tool per session, never a failure |
| `review-simplicity` | PostToolUse (Write\|Edit) | Advisory: feeds back ≤8 comment, complexity and smell findings the edit introduced |
| `memory-start` | SessionStart | Injects the bank's focus (Current Focus, Blockers, Next Steps, In Progress) plus a `memory search` pointer when `memory/index.md` exists; repairs `projects/<slug>/memory` (links to the store root); on `clear` reports the compaction record, the newest model-written handoff under 24 h, then the session's generated one; on `compact` injects this session's own steward handoff body first (budgeted within the 9,500-character context cap, with its path and a truncation notice), then the record and the pointers (`memory-bank.md` → Who writes what) |
| `paths-resolve` | UserPromptSubmit | Reports existing paths in the prompt; translates Windows ⇄ `/mnt/…` on WSL |
| `memory-recall` | UserPromptSubmit | Injects up to 2 bank bodies plus 3 pointers within 900 chars; skipped under 4 prompt terms; a hit must match 2-3 of them; logs hits to `state/harness/memory/access/<name>.log` |
| `codex-agent-route` · `codex-routing-result` · `codex-routing-start` · `codex-subagent-start` | codex agent events | Codex-only routing hooks; never in Claude settings |
| `memory-compact` | PreCompact | Concurrently dispatches a background update for uncovered edits (plus a record under `state/harness/compact/`) starts the steward handoff when this session has none fresh, bounds steward stamp-lock acquisition by the hook context, waits up to 75 s for a completed, full-session-verified one (budget 90 s, matching the settings timeout; a timeout or failure is a stderr line), and writes a fact-only handoff (git state, this session's dispatches, newest rulings, pointers) to `handoffs/YYYY-MM-DD-compact-<id8>.md`; either failing is a stderr line; never blocks |
| `memory-end` | SessionEnd | After enough edits spawns `claude-code memory update`; the detached `memory watch` (`memory-bank.md`) |
| `worktree-create` / `worktree-remove` | WorktreeCreate / WorktreeRemove | Create at `~/worktrees/<repo>/<name>` (honors `worktree.baseRef`, seeds `.venv`/`.env`, opens ledger `memory/worktrees/<name>.md`); remove refuses a dirty tree and `branch -d`s only a merged branch |
| `worktree-busy` ¶ | UserPromptSubmit | Vault env `HARNESS_WORKTREE` (`worktree-git.md` → Worktrees); drains this session's notices (≤5) and adds a divergence line in a diverged shared checkout |
| stuck-leaf watchdog | within `worktree-busy` (UserPromptSubmit) and `docs-plan-reminder` (Stop), main Claude session only | At most once a minute per session runs `agents watch`'s classifier over this session's jobs and adds one context line per stale job and reason, once: dead with its record running, alive past `HARNESS_WATCH_AGE` (10m; `Tier: fast` 0.5x, `deep` 2x) and silent past `HARNESS_WATCH_SILENCE` (5m), running after its relay finished, or finished 5m and uncollected (`dispatch-workflow` marks a job collected when a Bash call reads it); a dead job with a session id is resumed once (`agents ask --session <id> --background`, a fixed continuation prompt), never restarted or cancelled; a silent live job is only reported; vault env `HARNESS_WATCH=off\|false\|0` turns it off, `dry` names the resume without starting it; markers in `state/harness/watch/` |
| `worktree-claim` | PostToolUse (Write\|Edit\|MultiEdit; Bash async) | Appends written and staged paths still dirty after the call to `state/harness/hooks/claims/<pid>-<session>.jsonl`; each line records the owner's start token, so a file whose pid was reused is dropped; never blocks. Write\|Edit\|MultiEdit stays synchronous; Bash runs in the async entry, so a `settings.json` without it keeps Bash synchronous and a headless `claude -p` that exits first kills the run |
| `worktree-claim-check` ¶ | PreToolUse (Write\|Edit\|MultiEdit\|Bash) | Notes before an edit that a dirty file is claimed by another live session or sibling leaf; asks on a staging or commit command that includes one (`-p`, a dry run and `-N` pass), blocks in a leaf under `~/.claude`; infra errors allow with a warning |

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
