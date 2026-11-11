---
paths:
  - "internal/hooks/**"
  - "internal/settings/hookspec.go"
---
# Hooks System

Hooks run as `claude-code hook <Event> [--<module>|--all]`. **`internal/settings/hookspec.go` is the only place a
hook is declared** (one `Spec` per invocation); the handler lives in `internal/hooks/`. Add, remove or retime: edit
both, then `make install`. See `settings-management.md`.

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
| gemini (agy) · native | `permission-gate` only | **block** |
| opencode · hosted | `Hosted` specs (gates, docs, memory, format, review; no worktree notices) | PreToolUse **detect-only** (logged, cannot stop the call); the rest **advisory**; Stop and SessionEnd run only after a watched tool call |
| opencode · plugin | `safety-scan`, `permission-gate`, `docs-placement`, `docs-plan-gate`, `worktree-claim-check` | **block** (throws in `tool.execute.before`, headless and TUI; the host then skips these) |

The `permission-gate` module (AgentOnly `permission` group) judges each call with `Permissions.Decide` against
`HARNESS_DISPATCH_PERMISSIONS`, covering `bash -c`: opencode runs it from the plugin, codex and agy natively. Copilot
`ask` runs through the SDK, whose session loads `~/.copilot/hooks/*.json`, so the native row covers it. Skills,
instruction sources and the Bash-deny path: `agent.md` → Skills and instructions parity.

## Event → hook matrix

| Hook | Event | What it does |
| --- | --- | --- |
| `safety-scan` | PreToolUse (Bash) | **Block** destructive shapes (`rm -rf /`, a system dir or `$HOME` as the shell reads it: quoted, escaped, `//`, `..`; fork bomb; `curl \| sh` also by absolute shell path; `eval "$(curl …)"`; `base64 -d \| sh`) and a `commit --amend`/`rebase`/`reset <commit>` in a shared checkout (`~/.claude`, a tree a peer session works in, a main tree where this session has live leaves or the caller is a leaf; own linked worktree exempt; an infra error allows with a warning); **ask** on Claude (block on codex/copilot) for `git stash/checkout/switch/reset/restore/clean` under `~/.claude` (nested repos too) — linked worktrees exempt — and a commit with an AI-attribution footer, and on intent-dependent shapes (`sudo`, `DROP TABLE`, `rm -rf .git`, a force-push of main/master: spelled out, after git global flags, or with no refspec while main/master is checked out) naming the safer alternative; **warn** only prints. Every shape ignores inert text (commit/tag/`gh` messages, `echo`/`printf`/`grep`/`rg` arguments; file-only `cat`/`tee` heredoc bodies for the pipe, exfil and SQL shapes) unless it is piped on or holds a `$(…)`; a hard-block shape found only in such text asks instead of blocking, but any `<<` in the command scans the heredoc bodies too and blocks. Blocks log to `claude-code doctor --audit` by reason and pattern, never the command |
| `permission-gate` | PreToolUse (Bash\|Write\|Edit\|Read\|WebFetch\|Glob\|Grep) | **Block** a call outside the dispatch's `Permissions` (5s), codex's `apply_patch` judged as an Edit on each file its patch headers name; agent-only: codex, gemini, opencode plugin, never in Claude settings |
| `rtk-rewrite` | PreToolUse (Bash) | Rewrites a command via `rtk rewrite` (chains included; exit 0/3 with changed output; never sets a permission decision) and runs the pinned `bin/rtk` (else PATH); Claude sessions only. Vetoes: already wrapped, any `git` in a linked worktree, any `git commit`, machine-readable flags (`--porcelain`, `--json`), value subcommands (`rev-parse`), `go vet` (rtk 1.1.0 reports its failures as "No issues found"). Exact bytes: `rtk bypass <cmd>` (or the pinned path when PATH resolves another rtk); `HARNESS_RTK_BIN` overrides. A bare `rtk hook claude` entry in settings is stale and removed on `make install` |
| `docs-placement` | PreToolUse (Write\|Edit) | Blocks a new `.md` at a repo root or under a gitignored `docs/`, naming the home (`docs-placement.md`); also checks `apply_patch` headers |
| `docs-plan-gate` | PreToolUse (Write\|Edit) | Denies the first write to a native plan draft lacking `# Plan:`, `## Context`, `## Phases`, `## Tasks` from `docs/templates/plan.md`; the denial pastes the skeleton |
| `dispatch-effort` | PreToolUse (Agent\|Task\|Workflow) | In order: cap, type gate, model pin, slot claim. The type must be an `agents/<name>.md` file, `Explore` or `general-purpose`; any other type is blocked naming the roster. A brief's `Profile: <name>` keeps the dispatch's type (read-only types `Plan`/`Explore`/`gemini` and `Agent: in-context` briefs are left alone), labels the row, puts the `agents ask --profile` command in the prompt and denies an unknown name; `Route: databricks[:<profile>]` does the same with `--route`. Fills an UNSET model from a local classifier (low→haiku, else sonnet); an explicit `model:` (dispatch or frontmatter, `inherit` included) always wins. Appends ≤3 memory recall pointers (in auto mode, recall and label go to additionalContext and a by-name Workflow under the cwd is blocked with its scriptPath: the classifier can't review rewritten input). Workflows: pins `model: 'sonnet'` on every unpinned inline-`script` `agent()` (a `scriptPath` file is not rewritten, only blocked on a missing or bad pin), redirects a by-name call to its `scriptPath` (registry read once at session start), blocks an unrewritable options argument or a bad pin literal, and blocks bad `args.steps` (unknown stage/field, agent, Claude model, effort, plain names, profile on a Claude stage, `code-engineer` `child` against `code-workers`) or an external stage with no route (`agent.Resolve` preview) before any agent starts |
| (dispatch cap) | within `dispatch-effort` | **24 in flight per available account** (a profile's `concurrency` overrides), machine-wide `24 × accounts` clamped to **4 × CPUs**, test agents included. A blocked dispatch claims nothing; a Workflow claims one slot per `agent()` site; infra errors fail open. `claude-code doctor` shows the live `in-flight` count. The description starts `<model>: <task>` (in auto mode the hook denies once naming the exact text to resend, else it rewrites; `claude-code agents label <type> --brief FILE` prints the same prefix); the row shows the leaf type, which is the agent (`claude`, `copilot`, `codex`…) (`skills/subagent-workflows/SKILL.md` → Leaf types) |
| `dispatch-failure` · `dispatch-workflow` · `dispatch-stop` | PostToolUseFailure · PostToolUse (Workflow) · SubagentStop | Retire the spawn's slots. `dispatch-stop` first blocks a leaf's stop (up to 2 hours from the dispatch's start, then a visible fail-open) while a `job <id> started` its own transcript printed is still running, naming `claude-code agents tail -f <id>`; a blocked stop retires nothing. A `codex`/`copilot`/`gemini`/`opencode` leaf is also blocked up to 3 times at a stop whose tool results carry no `ask: ran on <agent> ·` line or `ran_on` field (`claude` leaves and `Agent: in-context` briefs are exempt; the type is the request; prior blocks are counted from hook errors and feedback in its transcript); the 4th stop passes with a visible message. A stop it isn't blocking kills the leaf's leftover background shells (its own task ids only) |
| `dispatch-handback` | PreToolUse (SubagentHandback) | Runs `dispatch-stop`'s two leaf gates (ran-on: `codex\|copilot\|gemini\|opencode`) on the hand-back call, which ends a background leaf before SubagentStop can block it: a live dispatch denies until it ends (2 h cap); a missing ran-on line denies up to 3 times |
| `docs-plan-mirror` | PostToolUse (Write\|Edit) | Mirrors the undated draft to a dated, repo-tagged file from the first write (slug from `Suggested slug:`, else the H1); edits keep destination and date. A shared mirror lock serializes destination selection and writing; a failed destination record drops the empty claim and reports the error |
| `docs-plan-save` | PostToolUse (ExitPlanMode) | Files approved text at the mirror destination (or files a response-only plan), adds a missing checklist, verifies, removes the draft |
| `docs-plan-tick` / `docs-plan-reminder` | PostToolUse (Bash) / Stop | A landed commit ticks the plan tasks its `owns:` covers (`claude-code plan status`), in the newest open plan it touches (an older plan only with a live claim); `git commit -q` prints no sha, so it ticks nothing (`plan tick --range`); partly landed tasks are reported once at Stop under one shared partials lock, not per-session locks. `claude-code clean` sweeps obsolete `*.partial.lock` files older than one day |
| `memory-update` | Stop | Context steward first: at ≥`HARNESS_CONTEXT_STEWARD_PCT` context (default 75; `0` mutes only the prompt) it spawns a detached `claude-code memory update` and asks for a handoff then `/compact`, once per crossing and only with no background task, subagent marker or live dispatch (a dip below the threshold re-arms; never blocks Stop). Then the memory update, weighing edits and user turns (2 turns = 1 edit); with no spawn possible it blocks ≤3 times, then prints once that the update was NOT saved |
| `format-stop` | Stop | Formats files this session edited within its cwd subtree that are still dirty and unformatted, at most 50 per Stop, newest file mtime first: the repo's `make format\|fmt` if it takes a per-file variable, else the pyproject python chain, else gofmt/ruff/prettier (only with a prettier config or `package.json` entry)/shfmt |
| `review-simplicity` | PostToolUse (Write\|Edit) | Advisory, never blocks: feeds back ≤8 comment, complexity and smell findings the edit introduced (not ones already in the file) |
| `memory-start` | SessionStart | Injects a pointer to `memory/index.md` + this repo's focus; repairs `projects/<slug>/memory`; bank scaffolding, the daily `memory dream` and the `memory catchup` sweep: `rules/workflows/memory-bank.md` → Who writes what; on `compact`/`clear` it also reports the compaction record and the newest handoff under 24 h old (user line and transcript) |
| `paths-resolve` | UserPromptSubmit | Reports existing paths in the prompt; translates Windows ⇄ `/mnt/…` on WSL |
| `memory-recall` | UserPromptSubmit | Injects ≤3 bank blocks matching ≥2 prompt terms (plus a repo store the prompt names); skips blocks in context; logs hits to `access.log` (the hot/cold signal for `memory review`) |
| `codex-agent-route` · `codex-routing-result` · `codex-routing-start` · `codex-subagent-start` | codex agent events | Codex-only routing hooks; never rendered into Claude settings |
| `memory-compact` | PreCompact | Dispatches a background update for uncovered edits and leaves a per-session record (trigger, context %, time) under `state/harness/compact/` for the next start; never blocks |
| `memory-end` | SessionEnd | After enough edits spawns `claude-code memory update`; the first dispatch claims one detached `memory watch` that updates after 10 quiet minutes, so a crash loses nothing |
| `worktree-create` / `worktree-remove` | WorktreeCreate / WorktreeRemove | Create at `~/worktrees/<repo>/<name>` (honors `worktree.baseRef`, seeds `.venv`/`.env`, opens ledger `memory/<repo>/worktrees/<name>.md`); remove refuses a dirty tree and `branch -d`s only a merged branch |
| `worktree-mode` / `worktree-busy` | SessionStart / UserPromptSubmit | Vault env `HARNESS_WORKTREE`: unset asks once at a fresh session start (not resume/clear/compact) and when a peer joins the checkout (not this session's own tree, a linked worktree, or an `agy --hub` daemon); `off`/`false`/`0` is silent. Never creates a worktree; no yes means keep working in place. `worktree-busy` also drains this session's notices (`state/harness/notices/`, ≤5) and, in a shared checkout diverged from its upstream, adds one line per HEAD/upstream pair, whatever the mode |
| `worktree-claim` | PostToolUse (Write\|Edit\|Bash) | Appends each written path, each path a `git add` left staged, or each sha a landed `git commit` printed, to `state/harness/hooks/claims/<pid>-<session>.jsonl`; never blocks |
| `worktree-claim-check` | PreToolUse (Write\|Edit\|Bash) | Once per file, notes before an edit that a dirty file is claimed by another live session (owner pid alive) or sibling leaf (transcript fresh) and not committed since; asks on a `git add`/`commit` that stages one (`-p` passes), blocks in a leaf; an infra error allows with a warning. Dead owners' files are removed; a file under this process's own pid (a new session slug after `/clear` or a resume) is ours, not a peer's. Any `git` command also gets the drained notices and divergence line |

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
