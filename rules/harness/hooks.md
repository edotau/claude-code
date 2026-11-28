---
paths:
  - "internal/hooks/**"
  - "internal/platform/settings/hookspec.go"
---
# Hooks System

`$HOME/.claude/bin/.hookd <Event> [--<module>|--all|--async]` runs hooks; `claude-code hook <Event>` also works.
`make compile` builds both with `bin/.build-stamp`; non-hook verbs or stamp mismatch hand over to claude-code.
Safety treats both binaries alike.

Declare hooks only in `internal/platform/settings/hookspec.go` (one Spec per invocation); handlers live in
internal/hooks. Change both, install projections, hand-edit existing Claude settings (`settings-management.md`).
`claude-code agents sync` renders native hooks/configs; `agents sync --check` detects drift.

## Groups and the dispatcher

`settings.Registry` defines run order; codex is agent-only.

| Group | Purpose |
| --- | --- |
| safety | Bash safety gate |
| rtk | Command-output rewriting; `rules/standards/rtk.md` |
| docs | Placement, plan template/copies/ticks |
| dispatch | Spawn model, slots, recall |
| paths | Windows/WSL prompt paths |
| memory | Focus/recall, handoff/update |
| format | Format this session's dirty edits |
| review | Advisory edit findings and R28 hand-back/decision handling |
| worktree | Create/remove, ledger, claims, notices |
| lane | Detached landing or queue |
| codex | Child routing and log |

- One synchronous event entry reads once, budgets groups, recovers panics and merges results.
  Module flags select one; all selects non-agent-only groups with applicability.
- Safety runs separately so other groups cannot delay a block.
- Async PostToolUse runs Bash plan ticks/claims after tool return; notes arrive later.
  Keep the sync Bash matcher for dispatch hooks; install the async entry per settings-management.
- No per-group switch: declared groups run. One-launch debug override: `HARNESS_HOOK_SKIP=<module>[,…]` or all
  in hook env; all spares safety-scan unless named. Fix narrowly or delete spec/handler; never skip via vault.

Telemetry records event/module/outcome/duration/session-id prefix/tool/rule/verdict, never command/path/brief/secrets.
Day files in `state/harness/hooks/` retain 14 days, cap at 16 MiB and mark dropped rows.
Doctor reports seven-day fires, denies, overruns and p95; a dropped tail is not evidence of inactivity.

## Applicability by agent

`settings.SpecsFor(agent, surface)` is authoritative: native hook files, hosted headless runs, in-process plugins.
Codex/Copilot/agy lack ask, so ask becomes deny; other hooks are advisory unless marked blocking.

- **Claude/native:** non-agent-only groups plus launch-settings web and safety-claude-edit; blocking pretools,
  context-injecting post/prompt hooks.
- **Codex/native:** safety, docs placement, claims/checks, paths, recall, rtk, codex routing, memory start/update/end.
  Pretools block; apply_patch claims too.
- **Copilot/native:** safety, docs placement, claims/checks, paths, recall, memory start/update/end and format-stop.
  Pretools block.
- **Gemini/agy/native:** safety, docs placement and claims/checks block; no rtk rewrite.
  Plan mirror/tick, simplicity, memory start/update and format/reminder also run.
  SessionStart injects a system step; Stop block means continue; PostToolUse notes do not reach the model.
- **OpenCode/hosted:** docs/paths/memory/format/review advisory; safety/placement run through the plugin.
  Pure mode is refused while a pretool gate is bound; Stop/SessionEnd require an observed tool call.
- **OpenCode/plugin:** safety/placement/claim-check block, then rtk rewrite; claims run detached after edits.

**Pi · plugin:** only safety-scan, through the guard's seven tools and `hook PreToolUse --pi-guard`.
It ignores `HARNESS_HOOK_*`, normalizes Pi paths and uses canonical payload fields with agent pi.
Only an allow response with its path runs; ask, malformed replies or errors block.
No permission prompts/sandbox; no built-in tools. Guard load failure leaves no tools, pinned Pi version and per-dispatch
load nonce are required, and package-added tools block; realpath matching handles symlinked run dirs.
Bash alone reaches safety-scan; docs-placement and claim-check are not Pi modules.

| Pi | bash | edit | write | read | grep | find | ls |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Hook | Bash | Edit | Write | Read | Grep | Glob | Read |

Normalize leading @, tilde and Unicode spaces; refuse file URLs/unreproducible forms.
Judge typed and on-disk spellings. Details: `rules/harness/agent.md` → pi.

## Event → hook matrix

Safety policy follows; mechanism details for named hooks are in
`skills/subagent-workflows/references/hook-details.md`.

`safety-scan` has five rules and applies them to every agent, leaves included. Rules 1 to 3 and 5 check
each Bash call:

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
5. Whole-suite test run (owner, 2026-11-26; `internal/hooks/safety/suitegate.go`): a whole-suite run (`go test ./...`,
   `<dir>/...`, `all` or a `$(…)` list; `make test|audit|verify`; `pytest`/`tox` over nothing, directories or globs;
   `npm|pnpm|yarn|bun test`, `run test*|vitest|jest`, `npx vitest|jest` over nothing or directories; `cargo|mvn|gradle
   test` unfiltered; `worktree gate --run` without `--scope`, `worktree land --full-verify`) blocks in any agent and in
   any linked worktree; it allows the main session in the main checkout and every narrowed run (`go test -run
   '^TestName$' ./internal/<pkg>`, one package, `pytest file.py::test`, `-k`, `-t`, a test file, `cargo test name`).

Claude asks; an agent without ask gets a block. A leaf gets no exception from any rule.
Commands are matched as invocations, not as data text. Blocks name the allowed route and the owner's `!`;
`claude-code doctor --audit` logs no command text.

- **safety-scan — PreToolUse:** enforces the five rules above.
- **rtk-rewrite — Bash PreToolUse:** `rules/standards/rtk.md`; rewrite only when all gates allow both forms.
  Fail open on veto/error/timeout; Codex/OpenCode env-altered calls pass. Pi code mode bypasses it.
- **bash-timeout — Claude Bash PreToolUse:** raise foreground ask/wait/result-follow timeout to job timeout +10s,
  capped at 600000ms; never lower a larger setting or block. Long waits need shorter polling.
- **docs-placement — write/edit/Bash PreToolUse:** `docs-placement.md`; ordinary leaf Bash blocks become notes,
  but Plan-agent placement still blocks. First plans need template headings.
- **dispatch-effort — Agent/Task/Workflow PreToolUse:** retype pi to coworker; Codemode:off selects Claude,
  on/only may select coworker. Header must occupy a whole line, not a command flag.
  Pin unpinned models/effort by TaskTier, correct labels, claim slots and add recall in one updatedInput.
- **dispatch cap / slots:** 24 per available account, machine-wide, capped at 4 × CPUs.
  Excess spawns are unrecorded, never blocked. Description is resolved `<model>: <task>`;
  `agents label <type>` computes it; auto labels peek.
- **dispatch-failure / dispatch-workflow / dispatch-stop — failure/post/stop:** retire slots, attach finished receipts
  or note running/missing external dispatches; never hold a leaf stop.
- **dispatch-handback — SubagentHandback PreToolUse:** wait at most 13s, attach finished receipts or a running note;
  never refuse or impose Bash poll limits. Named plan tasks get their allowlisted verify/scoped owned-file gate
  within the budget, with results attached.
- **docs-plan — one module on PostToolUse `Write|Edit|ExitPlanMode|Bash` (Bash async) and Stop**, by the tool it gets:
  Write/Edit mirrors to a dated repo-tagged file from first write (slug from Suggested slug or H1, shared lock);
  ExitPlanMode files the approved text, adds a missing checklist, verifies and removes the draft; Bash ticks commits per
  `worktree-git.md` rule 3. Verify in scratch; red stays open; coverage never ticks.
  Ready tasks get context, never a block. Autofill claims ready briefs while slots remain and sends one detached
  ask batch through overlay at sonnet tier (R35); `HARNESS_AUTOFILL=off` stops it. Its Stop slot hosts the watchdog.
- **memory-update — Stop:** never blocks; detached update plus handoff at `HARNESS_CONTEXT_STEWARD_PCT` (default 75,
  zero mutes), once per threshold crossing, at most three spawns; failed spawn names manual memory update.
- **format-stop — Stop:** session's dirty cwd edits, newest 50; repo format/fmt per file, pyproject chain,
  then gofmt/ruff/prettier/shfmt. Markdown uses mdsmith including plans/memory; missing tools only note once.
- **review-simplicity — Write/Edit post:** at most eight introduced comment/complexity/smell/Markdown findings.
- **review-handback / review-notified — Agent result / task notification:** last UNFIXED line drives fix-now context;
  none is quiet; absent line asks main to read. Never blocks.
- **review-grep-exit — Bash failure:** explain no-match exit 1 and count-check handling; never block.
- **review-decide — Stop:** main-only, once, on an unanswered fix/review question or unquoted fixed ask;
  stop_hook_active prevents loops (R28).
- **memory-start / memory-recall / memory-compact / memory-end:** lifecycle: `memory-bank.md` → Who writes what.
  Recall uses up to two bodies/three pointers in 900 chars, at least four prompt terms and two–three matching terms.
  Start lists up to five recent open plans; compact waits at most 75s for steward within its 90s budget.
- **msg-inbox / msg-inbox-start — prompt / SessionStart (gemini):** unread `agents msg` posts as context, marked
  peer information; none is silent; never blocks.
- **paths-resolve — prompt:** report existing paths; translate Windows/WSL.
- **codex-agent-route / codex-routing-result / codex-routing-start / codex-subagent-start:** Codex only.
- **secrets-out / secrets-end — Bash post / SessionEnd:** Claude-only, never block; mask vault credentials
  at equal byte length in output and ended transcripts/subagents/tool results; ended files wait 30s unmodified.
- **web-tavily — web post:** Claude launch settings only, never persisted; preserve domain filters.
  Databricks search already ran in proxy. Missing TAVILY_API_KEY/errors keep original results; 25s budget.
- **worktree-create / worktree-remove:** create under worktrees with baseRef, env seeds and ledger;
  remove only clean trees and merged branches.
- **worktree-clean-end — SessionEnd:** remove merged clean idle lanes that did work; keep fresh lanes, never block.
- **worktree-busy — prompt:** drain notices and report audits/divergence once; no peer-session notice.
- **worktree-fixnow — Stop/prompt/git notice:** main interactive only; unclaimed current-checkout failures are
  directives. Owner present within `HARNESS_FIXNOW_IDLE` (default 10m, typed prompts only): investigate,
  recommend and wait. Idle: fix, log found/done/rejected in memory reports, raise at next owner message.
  Deleting owner data or adding restrictions always waits. Never blocks tools; Stop combines block reasons.
- **stuck-leaf watchdog — busy/reminder:** main-only, at most once/minute; detect dead, overdue/silent,
  post-relay running or uncollected jobs. `HARNESS_WATCH_AGE` defaults 10m scaled by tier;
  `HARNESS_WATCH_SILENCE` defaults 5m. Resume a dead job with session id once, never restart/cancel it.
  `HARNESS_WATCH=off|false|0` disables; dry only names the resume. Healthy jobs get bounded progress context.
- **worktree-claim — edit/post Bash async:** record dirty written/staged paths in hook claims; never block.
  Codex/Copilot/Gemini run synchronously.
- **worktree-claim-check — edit/Bash pre:** note peer-held dirty edits; never refuse staging/commits.
  WORKTREE_MODE=lanes also notes tracked live-checkout edits; infra failures allow with warning.
- **lane-land — leaf stop / background job end (carried too) / prompt:** a detached worker judges the lane once the
  leaf or job finishes and queues it: ready (finished, `UNFIXED: none`, no safety/hookspec/settings path) or held
  with its reason (a cut-off job names `agents ask --retry <id> --recover --runner pi|cli`). It never touches the live
  checkout; every prompt lists queued lanes with their
  `worktree land --resolve-agent pi <lane>` command, and the main session lands them. See `worktree-git.md` → Worktrees.

**dispatch-effort and spawn mod:** `harness_spawn:ready` plus session id marks readiness; query computes changed
description/prompt/type/model without claims or recall. With the marker, pretools only emit recall, while required
type rewrites remain pretool (host validates types before spawn). Marker expires after seven days.
`mods/dispatch-spawn` loads with slash-usage via CLAUDE_CODE_PLUGIN_DIRS; doctor reports missing setup.
Without the mod, including headless/other CLIs, hooks rewrite normally.

## Authoring a gate

1. Trigger on checkable transcript/file facts, never self-report.
2. Name instructions and the evidence artifact in the block.
3. Release only on evidence (mtime, receipt, exit).
4. Cap Stop gates with visible fail-open. Input gates reject and allow retries.
   Infra fails open except workflow route-preview errors; Blocks hooks fail closed on panic, timeout or bad payload.

Test block, evidence-release and cap paths. Live probes read payloads from data files, not quoted command text;
tally allow/ask/block. Permission rules live in settings; ask beats allow.

## Hook performance and contract

Measure doctor p95 before/after. Automated hook work changes cost only (R27).

1. Loosen only; new Blocks hooks need owner approval (R13). Flag blocked leaves first; loosen for recurring false
   positives only (`skills/subagent-workflows/SKILL.md` → Error handling).
2. Auto-fix deterministic problems; deny only when code cannot decide.
3. Use narrow matchers and rare events; wildcard/per-edit/per-prompt processes cost every call.
4. Set Spec Budget (default 5s); use `core.HookContext(name)` and shorter I/O waits, degrading on expiry.
5. Prompt modules run concurrently; detach slow work and deliver notes later.
6. Cache content-derived results by file/commit/prompt hash under state; never cache decisions across sessions.
7. Independent handlers merge in declaration order; keep dependencies inside one module.
8. Only core.Result writes stdout; logs go to stderr. Follow host block encoding.
9. Be idempotent on retry/resume/concurrency; key appends by session.
10. Read credentials via vault verbs, never inherited env; log no command/path/brief/secret.
11. Failure policy: Authoring a gate, item 4.

Debug with doctor, telemetry and a saved-payload single-module replay; read status/output separately.
Use the one-launch override above only to diagnose, then fix/delete the module.
