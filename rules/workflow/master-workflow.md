---
paths:
  - "skills/subagent-workflows/**"
  - "commands/copilot.md"
  - "commands/code-workers.md"
  - "agents/*.md"
  - "workflows/*.js"
---
# Master Workflow — End-to-End Development Pipeline

> The canonical pipeline for every non-trivial task. Each phase gates the next.
> Skip phases only where explicitly noted.

## Pipeline Overview

```text
PROMPT --> CLASSIFY --> REVIEW --> CODEBASE INTEL --> RESEARCH --> PLAN
  --> [REFINE PROMPTS: opt-in] --> PARALLEL DISPATCH (shared tree) --> MERGE --> SIMPLIFY --> VERIFY --> COMMIT
```

| Phase | Command / Agent | Output | Gate |
|-------|----------------|--------|------|
| 0. Classify | (inline) | Scope tier | Determines which phases run |
| 1. Review | `Plan` subagent, or `claude-code ask --agent <leg>` for a second model family | Refined Task Brief | READY / NEEDS CLARIFICATION verdict |
| 2. Codebase Intel | 3 parallel `Explore` subagents, or one `gemini` leg (`claude-code gemini explore`) | Codebase Context Brief | Structure, conventions, deps covered |
| 3. Research | `gh` search, WebFetch, WebSearch, `claude-code gemini explore` (web research) | Prior art links + library refs | At least 1 relevant reference |
| 4. Plan | plan mode, or a `Plan` subagent (plan-only) | Implementation plan with task breakdown | User approval |
| 5. Refine *(opt-in)* | `general-purpose` subagent | Self-contained brief per sub-task | Each brief passes the cold-start test — **only with `--refine` on orchestrate** |
| 6. Dispatch | orchestrate (`subagent-workflows`), `/code-workers`, or `Agent` scoped to disjoint files in the shared tree | Completed sub-tasks | Each sub-task passes local checks |
| 7. Merge + simplify | `claude-code worktree merge`, then `Skill(simplify)` over the merged diff | Simplified changes on working branch | No merge conflicts; behavior gate green before and after simplify |
| 8. Verify | the `testing-guidelines` test-and-fix loop + the `problem-solving` gate chain | All gates green | format + lint + test + security pass |
| 9. Commit | `/copilot commit` (or plain git per `standards/git.md`) | Git commit on branch | Clean working tree |

---

## Phase 0: Classify Scope

| Scope | Signal | Pipeline |
|-------|--------|----------|
| TRIVIAL | Single-line fix, typo, rename | Execute directly (skip all phases) |
| LOW | Single function or file change | Phase 2 (intel) + 3 (research) + 8 (verify) |
| MEDIUM | 2-5 files, same module | Phase 1-4, 8-9 (sequential, no worktrees) |
| HIGH | Cross-module, 5+ files | Full pipeline, parallel dispatch |
| EPIC | Multi-session, architectural | Full pipeline, phased execution across sessions |

**Skip for**: typo fixes, single-line changes, git operations, documentation-only edits, or explicit
user override ("just do it").

---

## Phase 1: Adversarial Review

**Trigger**: MEDIUM+ scope.

Vet the *task request* before any code is written — in a `Plan` subagent, or on a different model
family via `claude-code ask --agent codex|gemini|opencode` (see `claude-code agents` for what is
available). Checklist, output format and verdict discipline: `skills/evaluate-code/references/task-review.md` (on
codex: `claude-code codex ask`).

The same adversarial pass run on the finished branch diff (verdict SHIP IT / NEEDS WORK / BLOCK) is the
pre-commit review around Phase 8 (`claude-code codex review [--adversarial] --base <branch>`).

---

## Phase 2: Codebase Intelligence

**Trigger**: All tasks except TRIVIAL, or when context is stale.

Launch **3 parallel `Explore` subagents** in a single message:

| Agent | Focus | Output |
|-------|-------|--------|
| Structure | Directory layout, entry points, pipeline stages | Package map + architecture overview |
| Conventions | Naming, error handling, logging, test patterns | Style rules + code references |
| Dependencies | Manifest deps, env vars, CLI tools, policies | Integration map + constraints |

**Output**: Codebase Context Brief — passed to ALL downstream agents.

**Skip if**: a context brief was generated this session and no files have changed.

**Scope the fan-out** to the directories the task touches — a repo-wide Explore sweep is the
biggest avoidable input-token cost of this protocol.

**Alternative**: one `claude-code gemini explore "<question>"` leg returns the same brief from agy
(code + web research), off Claude's token budget; `bridge` for whole-repo context.

---

## Phase 3: Research & Reuse

**Trigger**: Any new implementation (not pure refactoring).

1. `gh search code` / `gh search repos` — find existing implementations
2. `WebFetch` on vendor docs — confirm API behavior and patterns
3. `WebSearch` — broader discovery (only when 1-2 are insufficient)
4. Package registries (PyPI, npm, pkg.go.dev) — prefer battle-tested libraries
5. `claude-code gemini explore "<question>"` — one leg for web research with cited URLs, off
   Claude's token budget

**Gate**: At least 1 relevant reference found, or explicit "nothing found" documented.

---

## Phase 4: Plan

**Trigger**: MEDIUM+ scope.

Feed the Context Brief + Refined Task Brief into plan mode or a `Plan` subagent (write the plan, don't
implement). `agents/Plan.md` holds the plan template and phase rules.

1. Generate implementation plan with task breakdown
2. Identify dependencies between tasks (which can run in parallel)
3. Flag risks: migration needs, breaking changes, security surface
4. Mark each sub-task as `parallel-safe: true/false`

**Gate**: User approves the plan before proceeding.

---

## Phase 5: Prompt Refinement *(opt-in)*

**Trigger**: only when `--refine` is passed to orchestrate. Default flows go plan →
dispatch; use the refiner when sub-tasks go to fresh agents that need self-contained context.

Each refined brief carries: **Objective**, **Context** (files with paths and line numbers),
**Constraints** (CLAUDE.md rules relevant to this sub-task), **Implementation Steps**, **Acceptance
Criteria**, **Do NOTs**, **Verification Command** (exact `make`/`go test`/`pytest` command).

**Quality test**: `skills/subagent-workflows/SKILL.md` — "A fresh agent must be able to act from the
handoff alone."

**Output**: Dispatch Plan table:

| Task | Branch | Parallel-Safe | Dependencies |
|------|--------|--------------|--------------|
| Implement serializer | wt/task-1 | yes | none |
| Write API tests | wt/task-2 | yes | none |
| Update URL routing | wt/task-3 | no | task-1 |

---

## Phase 6: Parallel Dispatch

**Trigger**: 2+ parallel-safe sub-tasks.

- orchestrate (`subagent-workflows`) or `/code-workers parallel` — each sub-task as an `Agent` owning disjoint
  (ideally new) files in the shared tree; `copilot` in the SAME message (`standards/quality.md`).
- A second model family for one lane: `claude-code ask --agent <leg> "<brief>"` from Bash
  (`--format json` for a machine-readable envelope).
- Manual worktrees: `claude-code worktree create|list|merge|clean`.

Concurrency: lean leaves (`code-worker`, Sonnet) ~8–10 in flight; deep-context agents ≤8 (`agents.md`; the
`inflight` hook enforces it) — the input-tokens/min limit is the binding cap.

**Gate**: Each sub-task passes its own verification command before merge.

---

## Phase 7: Merge Back (`claude-code worktree merge`)

Merge and clean per `skills/using-git-worktrees` (its `references/merge-and-clean.md` holds the one
merge rule, the verify gate and conflict handling); never `stash`/`checkout`/`reset` the live tree
(CLAUDE.md hard rule 4).

**Simplify**: `Skill(simplify)` over the merged diff, before review and Phase 8 — parallel lanes duplicate each
other's helpers and only the merged diff shows it. A leaf gets `skills/simplify/references/leaf-brief.md`.

---

## Phase 8: Verify

**Trigger**: After all code changes, before commit. Loop the repo's gates by hand (or `Workflow gate-loop` when the
user asks for it).

The chain is `rules/standards/quality.md` → Verification Order; the loop, caps and report format are
`skills/problem-solving/references/verify.md`. Migrations are never edited to make a gate pass.

**Alternatives**: the codex final gate run (`claude-code codex ask --tools`) as the cross-model second
pass; the
`testing-guidelines` test-and-fix loop for targeted test fixing,
`skills/evaluate-code/references/techdebt.md` (scan and fix sections) for debt.

---

## Phase 9: Commit (`/copilot commit`)

Commit per `rules/standards/git.md` (stage named files, conventional message, confirm first);
`/copilot commit` is the executor, `/copilot pr` when a PR is requested.

---

## Quick Reference

| Scenario | Commands |
|----------|----------|
| Simple bug fix (LOW) | fix code -> the `testing-guidelines` test-and-fix loop -> `/copilot commit` |
| Feature in one module (MEDIUM) | review -> plan mode -> implement -> `/simplify` -> verify -> `/copilot commit` |
| Cross-module feature (HIGH) | review -> orchestrate `[--refine]` (simplify stage included) -> verify -> `/copilot commit` |
| Fix failing tests | the `testing-guidelines` test-and-fix loop |
| Clean up tech debt | `skills/evaluate-code/references/techdebt.md` (scan, then fix) |
| Review current changes | `/code-review` (quality) ‖ `/security-reviewer` |
| Second opinion from another model | `claude-code ask --agent <leg> "<review brief>"` |

## Agent Reference

Roster (`Explore`, `Plan`, `claude`, `code-worker`, `copilot`, `codex`, `gemini`, `opencode`) and
purposes: `rules/workflow/agents.md` → Available Agents. Phase mapping here: `Plan` → 1, 4;
`codex`/`gemini`/`opencode` → 1; `Explore` → 2; `general-purpose` (built-in) → 5, 6;
`code-worker`/`copilot` → 6; `claude` → 4, 7; `claude-code ask --agent <leg>` → 1, 6, 8.

> **Security review is a skill, not an agent.** Run it via `/security-reviewer` (the `security-reviewer`
> skill), typically alongside Phase 8.

## Saved Workflows (`workflows/*.js`)

Run one only when the user asks for it; the default dispatch is `/code-workers` ‖ `copilot` (Phase 6).
Deterministic templates of the phases above, runnable by name (`Workflow({name, args})`) or inline from
another script via `workflow(name, args)`. Sequential = awaited stages, Parallel = `parallel()`/`pipeline()`,
Loop = a bounded `while` — control flow is code, never a model decision. Agents work in the shared tree.
Every `agent()` call pins `model:` (or `agentType:`) AND `effort:` — an unpinned leaf inherits the session's
reasoning tier; a call that deliberately omits `effort:` carries `// lint:opus-ok` on its closing line. `meta`
is a pure literal and nothing throws at top level, or the saved-workflow registry skips the script silently.
`claude-code workflow lint` checks all three; `make test` gates the shipped scripts on it.

| Workflow | Shape | Phases | args |
|----------|-------|--------|------|
| `gate-loop` | Loop: gate → fix in scope → re-run, ≤N; exits `green\|stalled\|capped\|error`. Many repos: one call per repo, in parallel | 8 | `{gate?, cwd?, scope, maxIterations?, fixesPerIteration?}` |
| `implement-and-verify` | Parallel intel → implementer ‖ copilot → gate-loop. Does not chain into `audit-fix-verify` | 2, 6, 8 | `{task, cwd?, scope, packages?, gate?, maxIterations?, implModel?}` |
| `audit-fix-verify` | Sized scope → chunked auditors → batched refute → fix ‖ test per package → gate-loop → re-audit touched files (≤rounds). Loops rounds in-script (one level of `workflow()` nesting only); exits `dry\|no-fix\|stalled\|nothing-changed\|blocked\|capped\|error` | 2, 6, 8 | `{range \| paths \| findings (none = working-tree diff), cwd?, packages?, dimensions?, chunkChars?, fix?, minFix?, gate?, maxIterations?, rounds?, refuters?}` |
| `external-agent` | One codex/gemini/opencode/copilot call as a stage: smoke → JSON envelope → sonnet fallback | 1, 6, 8 | `{leg, cwd, task, smoke?, fallback?, tools?, timeoutMin?}` |

The skill A/B eval lives under evaluate-code: `skills/evaluate-code/scripts/code-evaluator.js`
(run via `scriptPath`, never by name) — `skills/evaluate-code/references/eval.md`.
