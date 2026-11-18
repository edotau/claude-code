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

Each module run appends one row to `state/harness/hooks/telemetry-YYYYMMDD.jsonl` (14 days, capped): event, module,
outcome, ms, 8-char session id, tool, and when a decision fired a `rule` and a `verdict` (`block`, `ask`, `rewrite`,
`held`, `refused`, `passed`); never command text, a path, a brief or a credential.
`claude-code doctor` reports seven days of fires, denies, budget overruns and p95 duration per module.

## Applicability by agent

`settings.SpecsFor(agent, surface)` answers "which hooks run where". Surfaces: `native` (the agent's own hook file:
Claude settings, `~/.codex/hooks.json`, `~/.copilot/hooks`, `~/.gemini/config/hooks.json`), `hosted` (the harness runs
the module around a headless `opencode run`), `plugin` (opencode in-process). None of codex, copilot or agy has an ask
decision, so an ask blocks there.

| Agent · surface | Modules | Mode |
| --- | --- | --- |
| claude · native | every group that isn't agent-only | **block** (`safety` asks only on destructive SQL and script writes); post/prompt hooks inject context |
| codex · native | `safety-scan`, `permission-gate`, `docs-placement`, `worktree-claim-check`, `paths-resolve`, `memory-recall`, `codex-*`; PostToolUse `worktree-claim` (`apply_patch` edits too); SessionStart `memory-start`; Stop `memory-update`; SessionEnd `memory-end` (3s cap) | **block** (an ask denies); the rest advisory |
| copilot · native | `safety-scan`, `docs-placement`, `worktree-claim-check`, `paths-resolve`, `memory-recall`; PostToolUse `worktree-claim`; sessionStart `memory-start`; Stop `memory-update`, `format-stop`; SessionEnd `memory-end` | PreToolUse **block** (an ask denies); the rest advisory |
| gemini (agy) · native | `safety-scan`, `permission-gate`, `docs-placement`, `worktree-claim-check`; PostToolUse `docs-plan-mirror`, `docs-plan-tick`, `review-simplicity`, `worktree-claim`; Stop `docs-plan-reminder`, `memory-update`, `format-stop`; SessionStart `memory-start` | PreToolUse **block** (an ask denies); SessionStart injects a system step; Stop block = `continue`; PostToolUse output is `{}`, so its notes never reach the model |
| opencode · hosted | `Hosted` specs (docs, paths, memory, format, review; `safety-scan` and `docs-placement` are declared hosted but fire only through the plugin; no worktree notices) | **advisory** (`--pure` loads no plugin, so it is refused while a PreToolUse gate is bound); Stop and SessionEnd run only after a watched tool call |
| opencode · plugin | `safety-scan`, `permission-gate`, `docs-placement`, `worktree-claim-check`; `worktree-claim` in `tool.execute.after` | **block** (throws in `tool.execute.before`; the host then skips these); the claim is a detached, non-blocking spawn per edit |

`permission-gate` judges each call with `Permissions.Decide` against `HARNESS_DISPATCH_PERMISSIONS`; how each agent
reaches it, and the gated Claude launches: `agent.md` → Bash deny via `permission-gate`.

## Event → hook matrix

Rows marked ¶ have full behavior text in `skills/subagent-workflows/references/hook-details.md`.

| Hook | Event | What it does |
| --- | --- | --- |
| `safety-scan` | PreToolUse (Bash) | **Block** destructive shapes (`rm -rf /`/`$HOME`, fork bomb, `curl | sh`, `base64 -d | sh`, a destructive `printf` piped to a shell), Bash writes to files the `Edit(...)` deny rules protect (a leaf, subagent or dispatched, can never write `settings.json` or `settings.local.json`, by Bash or an edit tool; a leaf also cannot set, unset or `env -u`-clear a `HARNESS_DISPATCH_*` variable, since a nested dispatch inherits its limits), `commit --amend`/`rebase`/`reset <commit>` and branch-ref moves or deletions that lose commits (`branch -f|-D|-M`, `update-ref`) in a shared checkout (a leaf counts as shared only under `~/.claude`, and the live-leaves trigger applies only there), a secret-reading command (`claude-code … --secret`, `gh auth token`, `env`) piped into `curl`/`nc`/`wget`, and (each naming the allowed route and the owner's `!`) `sudo` in command position, `git stash/checkout/switch/reset/restore/clean` under `~/.claude` (a new branch, an unstage, `stash list`, `clean -n`, linked worktrees and temp dirs pass), AI-attribution commit footers, `rm -rf .git` and a main/master force-push; **ask** on Claude (block on codex/copilot/agy) only for destructive SQL, a script writing a protected file and the main-session rules listed below the table; those rules fire only on a real invocation, never on words in data (a brief to `claude-code agents`, a heredoc written to a file, a plain Python stdin script, a grep/sed pattern, echo text, a commit message); hard blocks still scan every heredoc body and skip only the words of `claude-code agents`, `git commit|log`, `grep`/`rg`/`ag`, `jq`, `test` and an `echo`/`printf` not fed to a runner; a command written to a file and run in the same line is checked, an `apply_patch` call's patch text is not scanned; blocks log to `claude-code doctor --audit` (reason, pattern and session id, never the command). Full shape list: `skills/subagent-workflows/references/hook-details.md` |
| `permission-gate` | PreToolUse (Bash\|Write\|Edit\|Read\|WebFetch\|Glob\|Grep) | **Block** a call outside the dispatch's `Permissions` (5s); `apply_patch` is judged as an Edit per file; agent-only |
| `rtk-rewrite` ¶ | PreToolUse (Bash) | Rewrites a command via `rtk rewrite` (never sets a permission decision); Claude sessions only, skipped in auto mode; exact bytes: `rtk bypass <cmd>` |
| `docs-placement` | PreToolUse (Write\|Edit\|MultiEdit\|Bash) | Blocks a new `.md` at a repo root or under a gitignored `docs/`, naming the home (`docs-placement.md`); also checks `apply_patch` headers and Bash write targets; rewrites a new `*plan*.md` at a repo root or `docs/` to the dated plans home (denies with the fix where the host cannot rewrite); rewrites (with a note) a Claude first write to a native plan draft that lacks only `# Plan:` or `## Tasks`, and denies any other first write lacking the `docs/templates/plan.md` headings, listing them; `HARNESS_HOOK_SKIP=docs-placement` skips both |
| `dispatch-effort` ¶ | PreToolUse (Agent\|Task\|Workflow) | Gates type (rewrites `coworker:<agent>` to `<agent>` on every brief); pins model and effort by `agent.TaskTier` for an unpinned type; a `Run:` line's `--model auto` labels the model auto ranks; claims slots, recalls context (a context note in auto mode or on a `Run:` brief, else appended to the brief), for Codex-spawned children too (`ee4a67ce`); a dispatched `agents ask` carries the same block for every agent (`withRecall`, `internal/agent/ask/memory.go`; not on `--retry`, `--job`, `--safe-mode` or `--host`), labels the row; every fix rides one `updatedInput` with a note line each (a `Run:` prompt stays byte-identical); denies, naming the fix, a `Run:` line naming a missing workspace or (with `--fallback none`) an unserved model |
| dispatch cap | within `dispatch-effort` | **24 in flight per available account** (a profile's `concurrency` overrides), machine-wide `24 × accounts` clamped to **4 × CPUs**; a blocked dispatch claims nothing, infra errors fail open; `doctor` shows `in-flight`. The description starts `<model>: <task>`, the model the run resolves (`claude-code agents label <type> [--profile p] [--model m] --task FILE` prints it); a wrong or missing label is rewritten on every brief, never denied |
| `dispatch-failure` · `dispatch-workflow` · `dispatch-stop` ¶ | PostToolUseFailure · PostToolUse (Workflow\|Bash) · SubagentStop | Retire the spawn's slots; `dispatch-stop` blocks a leaf's stop while its job still runs (≤2 h); a `codex`/`copilot`/`gemini`/`opencode` leaf without an `ask: ran on <agent> ·` line is never refused: held while its job runs, then passed with the record's receipt; a never-run `Run:` line (no file edits; memory reports, handoffs and notes do not count) is run by the hook; relay work passes with a note; a held report is saved to the reports home; on Bash, `dispatch-workflow` (the synchronous entry, so the line reaches the model) gives a relay whose own `agents result` poll returned on an ended job the context line `job <id> ended: hand back now with this result` |
| `dispatch-handback` | PreToolUse (SubagentHandback\|Bash) | Runs `dispatch-stop`'s two leaf gates on the hand-back call, which ends a background leaf before SubagentStop; the note, receipt and job result are prepended/appended to the hand-back `message`; it waits up to 13 s for a running job and passes at once when it ends, else holds; a never-run `Run:` line with no file edits (memory reports, handoffs and notes do not count) is run by the hook, and relay work passes with a note; when a leaf started several jobs, every finished job's answer is attached in dispatch order, each under its own receipt line, including jobs that finished before the wait; only ids the leaf's own `agents ask\|dispatch\|result` call printed count, never ids from listings, logs, files or the answer body, except one its own successful `agents result\|wait <id>` named whose record is this session's, started within 2 min after the leaf's ask, with that ask's agent and a prompt holding its brief; with no such id it binds a record only when exactly one of this session's records of the ask's agent has a prompt equal to the ask's whole task (parsed by `ask`'s own grammar, flags anywhere, `--` ends them; a stdin task ties nothing) and launched within 2 min after it; otherwise the report passes with an unattributed note naming the candidate ids, never held or refused; a hold saves every finished answer and the later pass attaches them all; the hook's `Run:` launch is claimed with an exclusive create, so overlapping hand-backs start it once; every hold names `claude-code agents result -f <id>` to run once (never a sleep); on Bash it refuses a relay's poll of a finished job it already handed back (`already reported: stop`) and a second follow poll of one job while the first is in flight or backgrounded with a live writer (`one poller only`); a relay's Bash is `claude-code` asks, commands the shared read-only judge allows, and only `go build\|vet\|test` (behind `rtk`, a bare `env`, `time [-p]`, `nice [-n N]` or `timeout [opts] N[smh]`) with `./` package patterns inside the repo and a short flag list (`-args`, writing or program-running flags, any `go build` but `./...` or `-o /dev/null`, an inline or process `GOFLAGS`/`GOENV`/`GOWORK`/`GOBIN`/`GOCACHE`/`GOTOOLCHAIN` refused; the relay shell's own env is invisible to the hook) or `gofmt -l` |
| `docs-plan-mirror` | PostToolUse (Write\|Edit) | Mirrors the undated draft to a dated, repo-tagged file from the first write (slug from `Suggested slug:`, else the H1); a shared lock serializes it |
| `docs-plan-save` | PostToolUse (ExitPlanMode) | Files approved text at the mirror destination, adds a missing checklist, verifies, removes the draft |
| `docs-plan-tick` / `docs-plan-reminder` ¶ | PostToolUse (Bash, async) / Stop | A commit ticks exactly the open tasks its `Task: <plan-slug> <id>` trailers name (`worktree commit --task <id>`; only a real trailer counts: last paragraph, every line a column-zero trailer), at once, with a note listing any `owns:` file it left out. "Main" is the primary checkout with HEAD on the repo's trunk (origin/HEAD when it names an existing local branch, else `init.defaultBranch`, `main`, `master`; none exists, nothing ticks and one line says so); a lane, a feature branch or a detached HEAD ticks nothing, and a lane's tasks tick when `worktree merge` promotes them or the hook sees, in the HEAD reflog, a fast-forward or merge commit land on main, whatever the call printed or a later command did. Coverage without a trailer prints one candidate per task (recent or claimed plans only) and writes nothing; draft mirrors and struck tasks never tick. `claude-code plan task <id> --done --sha <sha> --plan <file>` stays manual and needs no trailer. `docs-plan-reminder` now hosts the Stop watchdog check and, in the main Claude session, one `systemMessage` line `<n> commit(s) on <branch> not pushed to <upstream>; done means pushed.` when a non-lane checkout's branch is ahead of its upstream (one `git for-each-ref`; once per branch and count in `state/harness/worktree-unpushed/<session>`; silent with no upstream, detached HEAD or a harness lane); its partly-landed record is no longer written. The Bash work runs in the async entry (`hook PostToolUse --async`): the tool returns at once and the note reaches the model a turn later; a `settings.json` without that entry keeps it synchronous, and a headless `claude -p` that exits first kills it |
| `memory-update` ¶ | Stop | Context steward at ≥`HARNESS_CONTEXT_STEWARD_PCT` (default 75; `0` mutes the handoff) makes the one detached update also write `handoffs/YYYY-MM-DD-session-<id8>.md` (the hook picks the path, kept in the marker), once per crossing; pending/running jobs are rechecked under the stamp lock and count as covered; no prompt, no continuation (compaction is `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE`); then the memory update; never blocks; a failed spawn prints a NOT-saved note naming `claude-code memory update` at once; each of the next two Stops retries the spawn once (3 spawns in all), a Stop whose retry succeeds is silent, and the note shows once more at the cap (SessionEnd/PreCompact retry too) |
| `format-stop` | Stop | Formats this session's still-dirty edited files in its cwd subtree (≤50, newest first): repo `make format\|fmt` with a per-file variable, else the pyproject chain, else gofmt/ruff/prettier/shfmt; a formatter that is not installed prints one note per tool per session, never a failure |
| `review-simplicity` | PostToolUse (Write\|Edit) | Advisory: feeds back ≤8 comment, complexity and smell findings the edit introduced |
| `memory-start` | SessionStart | Injects the bank's focus (Current Focus, Blockers, Next Steps, In Progress) plus a `memory search` pointer when `memory/index.md` exists; repairs `projects/<slug>/memory` (links to the store root); on `clear` reports the compaction record, the newest model-written handoff under 24 h, then the session's generated one; on `compact` injects this session's own steward handoff body first (budgeted within the 9,500-character context cap, with its path and a truncation notice), then the record and the pointers (`memory-bank.md` → Who writes what) |
| `paths-resolve` | UserPromptSubmit | Reports existing paths in the prompt; translates Windows ⇄ `/mnt/…` on WSL |
| `memory-recall` | UserPromptSubmit | Injects up to 2 bank bodies plus 3 pointers within 900 chars; skipped under 4 prompt terms; a hit must match 2-3 of them; logs hits to `state/harness/memory/access/<name>.log` |
| `codex-agent-route` · `codex-routing-result` · `codex-routing-start` · `codex-subagent-start` | codex agent events | Codex-only routing hooks; never in Claude settings |
| `memory-compact` | PreCompact | Concurrently dispatches a background update for uncovered edits (plus a record under `state/harness/compact/`) starts the steward handoff when this session has none fresh, bounds steward stamp-lock acquisition by the hook context, waits up to 75 s for a completed, full-session-verified one (budget 90 s, matching the settings timeout; a timeout or failure is a stderr line), and writes a fact-only handoff (git state, this session's dispatches, newest rulings, pointers) to `handoffs/YYYY-MM-DD-compact-<id8>.md`; either failing is a stderr line; never blocks |
| `memory-end` | SessionEnd | After enough edits spawns `claude-code memory update`; the detached `memory watch` (`memory-bank.md`) |
| `secrets-out` | PostToolUse (Bash) | Claude only: a vault credential value in stdout or stderr (exact and encoded forms, no shape pass) is replaced at the same length through `updatedToolOutput`, so the model and the transcript keep the mask; no match returns nothing; output under 12 bytes never opens the vault; a vault read not done within 3/4 of the 2 s budget masks credential shapes instead, with a stderr note and a user-visible message naming the cause (at most once a minute); a failed read leaves `state/harness/hooks/secrets-down`, and for the next minute calls skip the vault read; never blocks |
| `secrets-end` | SessionEnd | Claude only: starts a detached `hook SessionEnd --secrets-end-job` that masks the ended session's transcript, `subagents/` and `tool-results/` files in place at the same size once they sit unmodified 30 s; a path outside `projects/` or through a link is refused; one pass at a time (`state/harness/hooks/secrets-end.lock`), counts logged to `secrets-end.log` |
| `worktree-create` / `worktree-remove` | WorktreeCreate / WorktreeRemove | Create at `~/worktrees/<repo>/<name>` (honors `worktree.baseRef`, seeds `.venv`/`.env`, opens ledger `memory/worktrees/<name>.md`); remove refuses a dirty tree and `branch -d`s only a merged branch |
| `worktree-busy` ¶ | UserPromptSubmit | Vault env `HARNESS_WORKTREE` (`worktree-git.md` → Worktrees); drains this session's notices (≤5) and adds a divergence line in a diverged shared checkout |
| stuck-leaf watchdog | within `worktree-busy` (UserPromptSubmit) and `docs-plan-reminder` (Stop), main Claude session only | At most once a minute per session runs `agents watch`'s classifier over this session's jobs and adds one context line per stale job and reason, once: dead with its record running, alive past `HARNESS_WATCH_AGE` (10m; `Tier: fast` 0.5x, `deep` 2x) and silent past `HARNESS_WATCH_SILENCE` (5m), running after its relay finished, or finished 5m and uncollected (`dispatch-workflow` marks a job collected when a Bash call reads it); a dead job with a session id is resumed once (`agents ask --session <id> --background`, a fixed continuation prompt), never restarted or cancelled; a silent live job is only reported; vault env `HARNESS_WATCH=off\|false\|0` turns it off, `dry` names the resume without starting it; markers in `state/harness/watch/` |
| `worktree-claim` | PostToolUse (Write\|Edit\|MultiEdit; Bash async) | Appends written and staged paths still dirty after the call to `state/harness/hooks/claims/<pid>-<session>.jsonl` (codex `apply_patch` headers, Copilot `toolArgs.path` and OpenCode `args.filePath` included; a move claims both names, a delete its path); each line records the owner's start token, so a file whose pid was reused is dropped, and a non-Claude agent's line inside a dispatch carries the dispatch id as `agent_id`, (read from the running dispatch record when the hook lacks the env), and `worktree commit` there treats a claim stamped with it, or filed by its own CLI process, as that leaf's own; an edit the vendor reports failed claims nothing; never blocks. Write\|Edit\|MultiEdit stays synchronous; Bash runs in the async entry, so a `settings.json` without it keeps Bash synchronous and a headless `claude -p` that exits first kills the run; Codex, Copilot and Gemini have no async entry, so it is synchronous there (about 10 ms per Bash or edit call) |
| `worktree-claim-check` ¶ | PreToolUse (Write\|Edit\|MultiEdit\|Bash) | Notes before an edit that a dirty file is claimed by another live session or sibling leaf; asks on a staging or commit command that includes one (`-p`, a dry run and `-N` pass), blocks in a leaf under `~/.claude`; vault env `HARNESS_WORKTREE=lanes` also asks (a leaf: blocks) on an edit, stage (`add -p` included), `git apply`/`am` or `git mv` of a tracked file in the live checkout (dry runs pass), naming `claude-code worktree create <slug>`, and leaves a lane alone; infra errors allow with a warning |

`safety-scan` main-session rules (a leaf is a subagent or a dispatched agent; found as commands, never as text).
On codex, copilot, gemini and opencode an ask blocks the main session too, naming the routes: a Claude session, where it
asks, or your own terminal.

- `claude-code … --secret` (prints a credential's value) on any verb: main session only, refused in a leaf; a leaf gets
  credentials at launch and reports the vault name it needs. `[--host|--remote h] vault get --key --secret` (the master
  key) is asked in the main session too. The redacted forms (no `--secret`) are open to all.
- `kubectl|oc exec|delete` (`oc rsh`) on production (`prod`/`prd`/`production` in a flag, `ENV=`, `KUBECONFIG=`,
  `use-context`, a deleted namespace or `current-context`; none found counts): asked in main, refused in a leaf.
- `make|gmake audit|install|fmt|build|verify|bootstrap|adopt` and a fixing `lint-<kind>` in the harness tree (install
  targets also in its linked worktrees): refused in a leaf (run `claude-code agents diff --check --path <files>` or
  `claude-code agents fix --path <files>`); `make lint` passes.
- A leaf's `HARNESS_DISPATCH_*` unset, `env -u` or assignment in text that reaches a shell, `eval`, `source`, an
  interpreter or `ssh` (its string, a here-string, a heredoc no pure reader takes, a pipe into it, a script it runs).

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

## Group summary

| Group | Does |
| --- | --- |
| `safety` | Bash safety gate (block / ask / warn tiers) |
| `permission` | Agent-only gate (codex, gemini, opencode): judges each call against the dispatch's permissions |
| `rtk` | Routes noisy command output through rtk |
| `docs` | Markdown placement gate; plan template, dated plan copies, plan-task ticks |
| `dispatch` | Concurrency cap (24 per account, ≤ 4 × CPUs), subagent model fill, memory pointers on spawn and on every `agents ask` |
| `paths` | Resolves Windows ⇄ `/mnt/…` paths in prompts |
| `memory` | Injects the catalog and recall at start/prompt; context checkpoint and memory update at Stop/SessionEnd/PreCompact |
| `format` | Formats files this session edited that are still dirty |
| `worktree` | Creates/removes under `~/worktrees/<repo>/`, keeps the ledger |
| `codex` | Codex only: child-agent routing and its log |

The `review` group adds an advisory `review-simplicity` finding after each edit.
