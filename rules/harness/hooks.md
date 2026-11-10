---
paths:
  - "internal/hooks/**"
  - "internal/settings/hookspec.go"
---
# Hooks System

Hooks run as `claude-code hook <Event> [--<module>|--all]`. **`internal/settings/hookspec.go` is the only place a
hook is declared** (one `Spec` per invocation; `copilot-bash` is the exception, its own constant in
`internal/hooks/safety/copilotbash.go`); the handler lives in `internal/hooks/`. Add, remove or retime: edit
both, then `make install`. See `settings-management.md`.

## Groups and the dispatcher

`settings.Registry` lists groups in run order: `safety`, `permission`, `rtk`, `docs`, `dispatch`, `paths`, `memory`,
`format`, `review`, `worktree`, `codex`; `permission` and `codex` are agent-only, never in Claude settings.
Settings carry **one dispatcher entry per event** (`hook <Event>`) matching the union of its groups' matchers; it
reads the payload once, runs each enabled group within its budget, recovers panics and merges results. `--<module>`
runs one module; `--all` runs every module on the event, gates included, but still skips agent-only and disabled
groups and applies each module's agent applicability.

- **`safety` keeps its own process** (`hook PreToolUse --safety-scan`) so a hung group can never delay or skip a
  block, and it cannot be switched off.
- **Switches:** only a non-gate group turns off, with `"<group>": false` under `harnessHooks` in the vault overlay
  (`claude-code vault edit`); an unknown group or a gate group (`safety`, `permission`) is rejected.
- **Agents:** codex, copilot and agy run only the modules in the table below, each as `hook <Event> --<module>`.
  None has an ask decision, so an ask blocks there.

## Applicability by agent

`settings.SpecsFor(agent, surface)` is the one answer to "which hooks run where"; surfaces: `native` (the agent's
own hook file: Claude settings, `~/.codex/hooks.json`, `~/.copilot/hooks`,
`~/.gemini/config/hooks.json`), `sdk` (copilot `ask` callbacks), `hosted` (the
harness runs the module around a headless `opencode run`), `plugin` (opencode in-process plugin).

| Agent · surface | Modules | Mode |
| --- | --- | --- |
| claude · native | every group that isn't agent-only (so not `permission` or `codex`) | **block** (`safety` also asks); post/prompt hooks inject context |
| codex · native | `safety-scan`, `permission-gate`, `docs-*` gates, `paths-resolve`, `memory-recall`, `codex-*` | **block** (an ask blocks) |
| copilot · native / sdk | `safety-scan`, `docs-*` gates, `paths-resolve`, `memory-recall`; Stop `memory-update`, `format-stop`; SessionEnd `memory-end` | PreToolUse **block** (an ask blocks); the rest advisory |
| gemini (agy) · native | `permission-gate` only | **block** |
| opencode · hosted | `Hosted` specs (gates, docs, memory, format, review, worktree) | PreToolUse **detect-only** (logged, cannot stop the call); the rest **advisory** |
| opencode · plugin | `safety-scan`, `permission-gate`, `docs-placement`, `docs-plan-gate`, `worktree-claim-check` | **block** (throws in `tool.execute.before`, headless and TUI; the host then skips these) |

The `permission-gate` module (AgentOnly `permission` group) judges each call with `Permissions.Decide` against
`HARNESS_DISPATCH_PERMISSIONS`, covering `bash -c`: opencode runs it from the plugin, codex and agy natively. Copilot
`ask` runs through the SDK, whose session loads `~/.copilot/hooks/*.json`, so the native row covers it. Skills,
instruction sources and the Bash-deny path: `agent.md` → Skills and instructions parity.

## Event → hook matrix

| Hook | Event | What it does |
| --- | --- | --- |
| `safety-scan` | PreToolUse (Bash) | **Block** destructive shapes (`rm -rf /`, force-push to main, `curl \| sh`) and a `commit --amend`/`rebase`/`reset <commit>` in a shared checkout (`~/.claude`, a tree a peer session works in, a main tree where this session has live leaves or the caller is a leaf; own linked worktree exempt; an infra error allows with a warning); **ask** on Claude (block on codex/copilot) for `git stash/checkout/switch/reset/restore/clean` at `~/.claude` — linked worktrees exempt — and a commit with an AI-attribution footer, and on intent-dependent shapes (`sudo`, `DROP TABLE`) naming the safer alternative; **warn** only prints. Pipe, exfil and SQL shapes ignore inert text (commit/tag/`gh` messages, `echo`/`printf`/`grep`/`rg` arguments, file-only `cat`/`tee` heredoc bodies) unless it is piped on or holds a `$(…)`; a destructive shape found only in such text asks instead of blocking. Blocks log to `claude-code doctor --audit` by reason and pattern, never the command |
| `permission-gate` | PreToolUse (Bash\|Write\|Edit\|Read\|WebFetch\|Glob\|Grep) | **Block** a call outside the dispatch's `Permissions` (5s); agent-only: codex, gemini, opencode plugin, never in Claude settings |
| `rtk-rewrite` | PreToolUse (Bash) | Rewrites a command via `rtk rewrite` (chains included; exit 0/3 with changed output; never sets a permission decision) and runs the pinned `bin/rtk` (else PATH); Claude sessions only. Vetoes: already wrapped, any `git` in a linked worktree, any `git commit`, machine-readable flags (`--porcelain`, `--json`), value subcommands (`rev-parse`). Exact bytes: `rtk bypass <cmd>` (or the pinned path when PATH resolves another rtk); `HARNESS_RTK_BIN` overrides. A bare `rtk hook claude` entry in settings is stale and removed on `make install` |
| `docs-placement` | PreToolUse (Write\|Edit) | Blocks a new `.md` at a repo root or under a gitignored `docs/`, naming the home (`docs-placement.md`); also checks `apply_patch` headers |
| `docs-plan-gate` | PreToolUse (Write\|Edit) | Denies the first write to a native plan draft lacking `# Plan:`, `## Context`, `## Phases`, `## Tasks` from `docs/templates/plan.md`; the denial pastes the skeleton |
| `dispatch-effort` | PreToolUse (Agent\|Task\|Workflow) | In order: cap, model pin, slot claim. A brief's `Profile: <name>` keeps the dispatch's type (read-only types `Plan`/`Explore`/`gemini`/`claude-code-guide` and `Agent: in-context` briefs are left alone), labels the row, puts the `agents ask --profile` command in the prompt and denies an unknown name. Fills an UNSET model from a local classifier (low→haiku, else sonnet); an explicit `model:` (dispatch or frontmatter, `inherit` included) always wins. At ≥90% Anthropic plan pressure downshifts an unset or frontmatter-pinned model one step (an explicit dispatch `model:` or a `Profile:` brief is never downshifted); at ≥97% moves `claude` to `copilot` (read-only `Explore` is never escalated) (`HARNESS_ROUTE_SUBAGENTS=0` disables). Appends ≤3 memory recall pointers (in auto mode, recall and label go to additionalContext and a by-name Workflow under the cwd is blocked with its scriptPath: the classifier can't review rewritten input). Workflows: pins `model: 'sonnet'` on every unpinned inline-`script` `agent()` (a `scriptPath` file is not rewritten, only blocked on a missing or bad pin), redirects a by-name call to its `scriptPath` (registry read once at session start), blocks an unrewritable options argument or a bad pin literal |
| (dispatch cap) | within `dispatch-effort` | **24 in flight per available account** (a profile's `concurrency` overrides), machine-wide `24 × accounts` clamped to **4 × CPUs**, test agents included. A blocked dispatch claims nothing; a Workflow claims one slot per `agent()` site; infra errors fail open. `claude-code doctor` shows the live `in-flight` count. The description starts `<model>: <task>` (in auto mode the hook denies once naming the exact text to resend, else it rewrites; `claude-code agents label <type> --brief FILE` prints the same prefix); the row shows the leaf type, which is the agent (`claude`, `copilot`, `codex`…) (`skills/subagent-workflows/SKILL.md` → Leaf types) |
| `dispatch-failure` · `dispatch-workflow` · `dispatch-stop` | PostToolUseFailure · PostToolUse (Workflow) · SubagentStop | Retire the spawn's slots. `dispatch-stop` first blocks a leaf's stop (up to 2 hours from the dispatch's start, then a visible fail-open) while a `job <id> started` its own transcript printed is still running, naming `claude-code agents tail -f <id>`; a blocked stop retires nothing. A `codex`/`copilot`/`gemini`/`opencode` leaf is also blocked once at a stop whose tool results carry no `ask: ran on` line or `ran_on` field (`claude` leaves and `Agent: in-context` briefs are exempt; the type is the request); the repeat stop passes with a visible message. A stop it isn't blocking kills the leaf's leftover background shells (its own task ids only) |
| `dispatch-handback` | PreToolUse (SubagentHandback) | Runs `dispatch-stop`'s two leaf gates (ran-on: `codex\|copilot\|gemini\|opencode`) on the hand-back call, which ends a background leaf before SubagentStop can block it: a live dispatch denies until it ends (2 h cap); a missing ran-on line denies once |
| `docs-plan-mirror` | PostToolUse (Write\|Edit) | Mirrors the undated draft to a dated, repo-tagged file from the first write (slug from `Suggested slug:`, else the H1); edits keep destination and date |
| `docs-plan-save` | PostToolUse (ExitPlanMode) | Files approved text at the mirror destination (or files a response-only plan), adds a missing checklist, verifies, removes the draft |
| `docs-plan-skeleton` / `-prompt` | PostToolUse (EnterPlanMode) / UserPromptSubmit | Injects the `docs/templates/plan.md` skeleton when plan mode starts, and once per plan on a plan-mode prompt |
| `docs-plan-tick` / `docs-plan-reminder` | PostToolUse (Bash) / Stop | A landed commit ticks the plan tasks its `owns:` covers (`claude-code plan status`), in the newest open plan it touches (an older plan only with a live claim); `git commit -q` prints no sha, so it ticks nothing (`plan tick --range`); partly landed tasks are reported once at Stop |
| `memory-update` | Stop | Context steward first: at ≥`HARNESS_CONTEXT_STEWARD_PCT` context (default 75; `0` mutes only the prompt) it spawns a detached `claude-code memory update` and asks for a handoff then `/compact`, once per crossing and only with no background task, subagent marker or live dispatch (a dip below the threshold re-arms; never blocks Stop). Then the memory update, weighing edits and user turns (2 turns = 1 edit); with no spawn possible it blocks ≤3 times, then prints once that the update was NOT saved |
| `format-stop` | Stop | Formats files this session edited that are still dirty and unformatted: the repo's `make format\|fmt` if it takes a per-file variable, else the pyproject python chain, else gofmt/ruff/prettier (only with a prettier config or `package.json` entry)/shfmt; a repo with its own pre-commit switches the group off |
| `review-simplicity` | PostToolUse (Write\|Edit) | Advisory, never blocks: feeds back ≤8 comment, complexity and smell findings the edit introduced (not ones already in the file) |
| `memory-start` | SessionStart | Injects a pointer to `memory/index.md` + this repo's focus (Current Focus/Blockers/Next Steps, In Progress); repairs `projects/<slug>/memory`; also scaffolds a missing bank, spawns the daily `memory dream` when due, and spawns one detached sweep (`memory catchup`) that saves up to 3 missed interactive sessions from the last 14 days, skips live ones, and prints one quiet line at the next start; on `compact`/`clear` it also reports the compaction record and the newest handoff under 24 h old (user line and transcript) |
| `paths-resolve` | UserPromptSubmit | Reports existing paths in the prompt; translates Windows ⇄ `/mnt/…` on WSL |
| `memory-recall` | UserPromptSubmit | Injects ≤3 bank blocks matching ≥2 prompt terms (plus a repo store the prompt names); skips blocks in context; logs hits to `access.log` (the hot/cold signal for `memory review`) |
| `codex-agent-route` · `codex-routing-result` · `codex-routing-start` · `codex-subagent-start` | codex agent events | Codex-only routing hooks; never rendered into Claude settings |
| `memory-compact` | PreCompact | Dispatches a background update for uncovered edits and leaves a per-session record (trigger, context %, time) under `state/harness/compact/` for the next start; never blocks |
| `memory-end` | SessionEnd | After enough edits spawns `claude-code memory update`; the first dispatch claims one detached `memory watch` that updates after 10 quiet minutes, so a crash loses nothing |
| `worktree-create` / `worktree-remove` | WorktreeCreate / WorktreeRemove | Create at `~/worktrees/<repo>/<name>` (honors `worktree.baseRef`, seeds `.venv`/`.env`, opens ledger `memory/<repo>/worktrees/<name>.md`); remove refuses a dirty tree and `branch -d`s only a merged branch |
| `worktree-mode` / `worktree-busy` | SessionStart / UserPromptSubmit | Vault env `HARNESS_WORKTREE`: unset asks once at a fresh session start (not resume/clear/compact) and when a peer joins the checkout (not this session's own tree, a linked worktree, or an `agy --hub` daemon); `off`/`false`/`0` is silent. Never creates a worktree; no yes means keep working in place. `worktree-busy` also drains this session's notices (`state/harness/notices/`, ≤5) and, in a shared checkout diverged from its upstream, adds one line per HEAD/upstream pair, whatever the mode |
| `worktree-claim` | PostToolUse (Write\|Edit\|Bash) | Appends each written path, each path a `git add` left staged, or each sha a landed `git commit` printed, to `state/harness/hooks/claims/<pid>-<session>.jsonl`; never blocks |
| `worktree-claim-check` | PreToolUse (Write\|Edit\|Bash) | Once per file, notes before an edit that a dirty file is claimed by another live session (owner pid alive) or sibling leaf (transcript fresh) and not committed since; asks on a `git add`/`commit` that stages one (`-p` passes), blocks in a leaf; an infra error allows with a warning. Dead owners' files are removed. Any `git` command also gets the drained notices and divergence line |

## Authoring a gate

Prose is the instruction; the hook is the guarantee. Every gate:

1. **Triggers on a checkable condition** — transcript facts or file state, never self-report.
2. **Blocks with instructions and the evidence contract** — what artifact proves compliance.
3. **Releases only on evidence** — mtimes, receipts, exit codes.
4. **Caps and fails open** — a Stop gate that can trap a session needs a cap and a visible fail-open; a
   deny-one-call gate (e.g. `docs-placement`) rejects the input and lets the model retry. Infra errors fail open;
   a blocking (`Blocks`) hook that panics or times out blocks.

Test the block path, the evidence-release path and the fail-open cap. Probe a live guard with a script that reads
payloads from a data file (quoted payloads in the probe command trip the guard itself) and tally allow, ask and
block separately. Permission guardrails live in the defaults'
`permissions` block (`ask` beats `allow`).
