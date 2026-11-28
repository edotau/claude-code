---
paths:
  - "**/*"
---
# Workflow and Worktrees

One process from request to commit: the pipeline says which phases a task runs, Worktrees says which tree each phase
works in and how lanes are collected and landed, LAST-MILE and the git rules cover the commit, and the last section
protects the shared checkout.

## Workflow

The pipeline for non-trivial work; dispatch follows `CLAUDE.md` guideline 3, brief and relay rules
`skills/subagent-workflows/SKILL.md`. Skip phases outside the requested scope, including commits for an edit-only
task. Phase 3 runs beside Phase 4 and joins at review.

```text
CLASSIFY → REVIEW → CODEBASE INTEL → RESEARCH → PLAN → [REFINE: opt-in] → DISPATCH → COLLECT → VERIFY → LAND → COMMIT
```

### Scope tiers (Phase 0, inline)

| Scope | Signal | Pipeline |
| --- | --- | --- |
| TRIVIAL | Single-line fix, typo, rename | Inline — the only inline scope; skip all phases |
| LOW | One function or file | Dispatch (not inline); phases 2, 3, 8 |
| MEDIUM | 2–5 files or needs tests, one module | Phases 1–4, 8–10; fan out via 6 whatever is unblocked; worktree per Worktrees below |
| HIGH | Cross-module, 5+ files | Full pipeline, parallel dispatch |
| EPIC | Multi-session, architectural | Full pipeline, phased across sessions |

TRIVIAL with no workflow request still goes inline. Which tree a tier uses follows Worktrees below: the number of
parallel leaves decides, not the tier.

### Phases

| Phase | Agent / command | Output | Gate |
| --- | --- | --- | --- |
| 1. Review (MEDIUM+) | `Plan`, or a carried `codex` ask | Refined Task Brief | READY / NEEDS CLARIFICATION |
| 2. Codebase intel | Scoped `Explore` leaves, or one carried `gemini` sweep | Codebase Context Brief | Structure, conventions, deps |
| 3. Research (new code) | `gh search`, `skills/web-explorer/SKILL.md`, or a carried `gemini` ask | Prior art + library refs | ≥1 reference, or "nothing found" |
| 4. Plan (MEDIUM+) | `Plan` | Plan with task breakdown | User approval |
| 5. Refine *(opt-in)* | `general-purpose` | Self-contained brief per sub-task | Cold-start test |
| 6. Dispatch | `skills/subagent-workflows/SKILL.md`; `claude-code worktree create` per Topology | Completed sub-tasks | Each leaf has handed back |
| 7. Collect | `claude-code worktree collapse <collector> <lane>` (if lanes were used) | One collector holding every lane | No leaf still running |
| 8. Verify | `quality.md` → Verification Order, on the collector | Simplified, verified diff | Order green |
| 9. Land | `claude-code worktree land <collector>`, then `worktree clean` (if a worktree was used) | Landed diff, uncommitted | Clean, no leaf still running |
| 10. Commit | Main session, only when authorized: LAST-MILE below | Commit pushed | Clean tree, pushed |

- **1 Review** vets the request before code: checklist and verdicts `skills/evaluate-code/references/task-review.md`,
  agent choice `skills/subagent-workflows/references/cost-routing.md`. The same pass on a finished diff is
  `claude-code codex review [--adversarial] --base <branch>`, around Phase 8.
- **2 Intel** is three parallel `Explore` leaves (Structure, Conventions, Dependencies) merged into one brief for every
  downstream agent. Scope them to the directories the task touches: a repo-wide sweep is the biggest avoidable token
  cost. Skip if a brief exists this session and nothing changed.
- **3 Research** order: `gh search code|repos`, web search via `skills/web-explorer/SKILL.md`, package registries
  (prefer battle-tested libraries). Start the research (a carried `gemini` ask, fallback a `claude` leaf) and `Plan` in
  one message, in the background; relay findings to the running `Plan` with `SendMessage`, or merge at review.
- **4 Plan** (rules `agents/Plan.md`, shape `docs/templates/plan.md`; multi-part work plans first, R18): review the
  draft before asking approval — in-context for MEDIUM, a carried `codex` ask for HIGH+:
  `skills/evaluate-code/references/task-review.md` → Plan review.
- **5 Refine**: one brief per sub-task with the fields in `skills/subagent-workflows/SKILL.md` → Handoff contract.
- **6 Dispatch** (Databricks arm, R20): shapes `skills/subagent-workflows/references/patterns.md`; a plan's
  `Phase W.k` waves run with `claude-code agents ask --waves <plan>` (never commits;
  `skills/subagent-workflows/references/agent-verbs.md`).
- **7 Collect and 9 Land**: worktree choice per Worktrees below, commands `skills/using-git-worktrees/SKILL.md`.
  Stop any leaf still running on merged work; a leaf at its turn limit reports partial work without committing.
- **8 Verify**: loop and caps `skills/problem-solving/references/verify.md`. Simplify runs once over the merged diff
  (parallel lanes duplicate helpers); before any new test, add nothing if tests already cover the change, delete dead
  code and stale tests it left, fold near-duplicate tests. **Never edit a migration to make a gate pass.**
- **10 Commit** only when authorized: LAST-MILE below and `CLAUDE.md` hard rule 5; the main session owns release
  checks and push.

Tech debt: `skills/evaluate-code/SKILL.md` → Tech debt.

## Worktrees

**Default is the current branch, with no worktree.** Worktrees exist only for parallel subagents. Vault env
`WORKTREE_MODE=off|false|0` (case-insensitive) always uses the current branch; `lanes` uses lanes for parallel work
without asking; unset means ask the owner once before the first change whether parallel work may use lanes.
With no parallel work, the live checkout is the only tree and there is no worktree, including in `lanes` mode.
Command flags, seeding and recovery: `skills/using-git-worktrees/SKILL.md`.

- **Topology:** `n = 1` creates one collector (`claude-code worktree create <name>` from the working branch); `n > 1`
  creates one collector plus one lane per parallel subagent from the collector's head, never from `main`.
- **Lane hand-back:** each subagent edits only its owned files and runs only cheap checks (`testing-guidelines.md`).
  When commits are authorized, Claude leaves commit named files in-lane (`worktree commit --task <id> -m <msg>
  <files>`); relay types (`codex`, `copilot`, `gemini`, `opencode`, `coworker`) hand back uncommitted for main to commit
  in-lane. An owner's "no commits" overrides both: hand back and land uncommitted.
- **Collect, land and clean:** as each lane finishes, main runs `worktree collapse <collector> <lane>` (no per-lane
  review or tests; resolve conflicts keeping both intents). Once collected, follow `testing-guidelines.md` → Several
  worktrees: collapse, then test once; `worktree land <collector>` rebases, gates in the collector, and applies the
  net diff uncommitted to the live checkout (a diff adding a restriction waits for the owner's yes). After the diff is
  in `HEAD`, run `worktree clean <name>`; no finished worktree outlives its task.

## LAST-MILE

Stop at the requested boundary: an edit or review is not authorization to land or commit (`CLAUDE.md` hard rule 5).
For an authorized landing, inspect the tree before resuming and follow Worktrees above; testing belongs only at
`testing-guidelines.md` → Several worktrees: collapse, then test once, not in a second post-landing gate chain.

Before committing, separate your changes from pre-existing dirt and inspect the selected and staged diffs for
secrets. Leave unrelated index entries untouched and report them under `excluded`. Use the owner's commit message;
otherwise confirm a Conventional message first. Inspect the actual commit message before reporting no attribution.
Report status, scoped gate results, `excluded`, blockers, resulting tree state and the commit hash if one was made.

## Git rules

1. **Conventional Commits** — `<type>[scope][!]: <description>`; types: `feat`, `fix`, `docs`, `style`, `refactor`,
   `perf`, `test`, `chore`, `ci`, `build`, `revert`. A subject over 150 characters warns; 2 lines max plus a
   `BREAKING CHANGE:` line. Use `Fixes #42` where an issue exists; a body line only for what the diff cannot show.
   No AI-attribution footers in commits, merges, tags or PRs (`safety-scan` blocks them).
2. **Branches** — `feature/<domain>-<component>`, `fix/<issue>-<description>`, `docs/<component>`,
   `hotfix/<issue>-<description>`, `test/<feature>`; delete after merging, subject to Worktrees' cleanup rule above.
3. **Stage named files only** — never `git add -A`, `git add .` or a directory. Commit with
   `claude-code worktree commit --task <id> -m <msg> <files>`, explicitly naming every file. Commit only when the
   request or approved wave plan authorizes it; otherwise confirm first.
   Plan completion: `standing-requirements.md` → R1.
   **An owner's "no commits" means nothing commits anywhere, lanes included; hand back and land uncommitted.**
4. **Atomic commits:** one logical change each. Never force-push a shared branch, rewrite published commits or skip
   gates. Read scoped checks' exit codes before pushing; chain with `&&`, never `;`. A docs commit requires a clean
   `claude-code agents diff --check`. Push only as directed by `CLAUDE.md` hard rule 5.

## Shared checkout

Never `git stash` in any worktree: the stash list is shared. In `~/.claude`, never stash, checkout, switch, reset,
restore, clean, rebase or amend a commit. Never amend, rebase or reset commits in a shared checkout; rewrite history
only in your own linked worktree. Never reset or rebase `main`. A new branch, unstaging named files and dry runs are
fine. Coordination: `skills/using-git-worktrees/references/shared-checkout.md`.

**A change you did not make is the owner's.** An edit, new file or deletion you did not make in this task belongs
to the owner or a peer: never revert, overwrite, reformat or "clean up" it, even when it fails a gate. Build on it
and report blockers. A peer's edits or claims get a notice, never a staging or commit refusal. Act on peer or merge
context (`notice (…)`, `local <branch> diverged`) before committing.
