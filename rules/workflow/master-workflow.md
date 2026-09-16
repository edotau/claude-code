---
paths:
  - "commands/workflow/*.md"
  - "commands/testing/*.md"
  - "commands/github.md"
  - "commands/code-workers.md"
  - "agents/*/agent.md"
  - "workflows/*.js"
---
# Master Workflow — End-to-End Development Pipeline

> The canonical pipeline for every non-trivial task. Each phase gates the next.
> Skip phases only where explicitly noted.

## Pipeline Overview

```
PROMPT --> CLASSIFY --> REVIEW --> CODEBASE INTEL --> RESEARCH --> PLAN
  --> [REFINE PROMPTS: opt-in] --> PARALLEL DISPATCH (worktrees) --> MERGE --> VERIFY --> COMMIT
```

| Phase | Command / Agent | Output | Gate |
|-------|----------------|--------|------|
| 0. Classify | (inline) | Scope tier | Determines which phases run |
| 1. Review | `Plan` subagent, or `claude-code ask --agent <leg>` for a second model family | Refined Task Brief | READY / NEEDS CLARIFICATION verdict |
| 2. Codebase Intel | 3 parallel `Explore` subagents | Codebase Context Brief | Structure, conventions, deps covered |
| 3. Research | `gh` search, WebFetch, WebSearch | Prior art links + library refs | At least 1 relevant reference |
| 4. Plan | plan mode, or a `Plan` subagent (plan-only) | Implementation plan with task breakdown | User approval |
| 5. Refine *(opt-in)* | `general-purpose` subagent | Self-contained brief per sub-task | Each brief passes the cold-start test — **only with `--refine` on `/workflow:orchestrate`** |
| 6. Dispatch | `/workflow:orchestrate`, `/code-workers`, or `Agent` with `isolation: "worktree"` | Completed sub-tasks | Each sub-task passes local checks |
| 7. Merge | `/workflow:worktree merge` | Changes on working branch | No merge conflicts |
| 8. Verify | `/testing:test-and-fix` + the `approval-gate` skill | All gates green | format + lint + test + security pass |
| 9. Commit | `/github commit` (or plain git per `standards/git.md`) | Git commit on branch | Clean working tree |

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
available):

1. **CAPTURE** the task description from user input
2. **CONTEXT** — read CLAUDE.md, `git status --short`, `git diff --stat`
3. **EVALUATE**: scope clarity, acceptance criteria, constraints, security surface, testing
   expectations, missing context
4. **OUTPUT** a Refined Task Brief with verdict: **READY** (proceed) or **NEEDS CLARIFICATION**
   (ask specific questions, loop back)

The same adversarial pass run on the finished branch diff (verdict SHIP IT / NEEDS WORK / BLOCK) is the
pre-commit review around Phase 8.

---

## Phase 2: Codebase Intelligence

**Trigger**: All tasks except TRIVIAL, or when context is stale.

Launch **3 parallel `Explore` subagents** in a single message:

| Agent | Focus |
|-------|-------|
| Structure | Directory layout, entry points, pipeline stages |
| Conventions | Naming, error handling, logging, test patterns |
| Dependencies | Manifest deps, env vars, CLI tools, policies |

**Output**: Codebase Context Brief — passed to ALL downstream agents.

**Skip if**: a context brief was generated this session and no files have changed.

---

## Phase 3: Research & Reuse

**Trigger**: Any new implementation (not pure refactoring).

1. `gh search code` / `gh search repos` — find existing implementations
2. `WebFetch` on vendor docs — confirm API behavior and patterns
3. `WebSearch` — broader discovery (only when 1-2 are insufficient)
4. Package registries (PyPI, npm, pkg.go.dev) — prefer battle-tested libraries

**Gate**: At least 1 relevant reference found, or explicit "nothing found" documented.

---

## Phase 4: Plan

**Trigger**: MEDIUM+ scope.

Feed the Context Brief + Refined Task Brief into plan mode or a `Plan` subagent (write the plan, don't
implement). The `planning` skill holds the task-breakdown rules.

1. Generate implementation plan with task breakdown
2. Identify dependencies between tasks (which can run in parallel)
3. Flag risks: migration needs, breaking changes, security surface
4. Mark each sub-task as `parallel-safe: true/false`

**Gate**: User approves the plan before proceeding.

---

## Phase 5: Prompt Refinement *(opt-in)*

**Trigger**: only when `--refine` is passed to `/workflow:orchestrate`. Default flows go plan →
dispatch; use the refiner when sub-tasks go to fresh agents that need self-contained context.

Each refined brief carries: **Objective**, **Context** (files with paths and line numbers),
**Constraints** (CLAUDE.md rules relevant to this sub-task), **Implementation Steps**, **Acceptance
Criteria**, **Do NOTs**, **Verification Command** (exact `make`/`go test`/`pytest` command).

**Quality test**: a fresh agent with zero prior context can execute the brief cold.

**Output**: Dispatch Plan table:

| Task | Branch | Parallel-Safe | Dependencies |
|------|--------|--------------|--------------|
| Implement serializer | wt/task-1 | yes | none |
| Write API tests | wt/task-2 | yes | none |
| Update URL routing | wt/task-3 | no | task-1 |

---

## Phase 6: Parallel Dispatch

**Trigger**: 2+ parallel-safe sub-tasks.

- `/workflow:orchestrate` or `/code-workers parallel` — each sub-task as an `Agent` with
  `isolation: "worktree"` (or exclusive new-file ownership); a test agent in the SAME message
  (`standards/quality.md`).
- A second model family for one lane: `claude-code ask --agent <leg> "<brief>"` from Bash
  (`--format json` for a machine-readable envelope).
- Manual worktrees: `/workflow:worktree create|list|merge|clean`.

Concurrency: lean leaves (`code-workers`, Sonnet) ~8–10 in flight; deep-context agents ≤4 — the
input-tokens/min limit is the binding cap.

**Gate**: Each sub-task passes its own verification command before merge.

---

## Phase 7: Merge Back (`/workflow:worktree merge`)

For each completed worktree, in dependency order:

1. **Preview**: `git log wt/<name> --oneline`
2. **Merge**: `git merge wt/<name> --no-ff`
3. **Conflict check**: resolve manually, then re-verify
4. **Clean**: `git worktree remove .claude/worktrees/<name>`

---

## Phase 8: Verify

**Trigger**: After all code changes, before commit. Loop (max 5 iterations):

```
Gate 1: <format>          -- auto-fix formatting
Gate 2: <lint>            -- fix lint errors
Gate 3: <test>            -- fix test failures (one at a time, max 3 attempts each)
Gate 4: security scan     -- hardcoded secrets, injection, unsafe logging (/security)
```

`<format>`/`<lint>`/`<test>` are the repo's real targets — discover them from the Makefile /
`pyproject.toml` / `package.json`. Scoped tests during development; the full suite once
(`standards/quality.md`).

**Escalation**: skip a gate after 2 failed fix attempts; stop if a migration is needed (the user runs
it); never modify `migrations/` directories.

**Alternatives**: `/testing:test-and-fix` for targeted test fixing, `/harness:techdebt scan` for debt.

---

## Phase 9: Commit (`/github commit`)

1. Stage changed files by name (never `git add -A`)
2. Conventional commit message: `<type>: <description>`
3. Confirm with the user before committing/pushing
4. Create a PR if requested (`/github pr`)

---

## Quick Reference

| Scenario | Commands |
|----------|----------|
| Simple bug fix (LOW) | fix code -> `/testing:test-and-fix` -> `/github commit` |
| Feature in one module (MEDIUM) | review -> plan mode -> implement -> verify -> `/github commit` |
| Cross-module feature (HIGH) | review -> `/workflow:orchestrate [--refine]` -> `/workflow:worktree merge` -> verify -> `/github commit` |
| Fix failing tests | `/testing:test-and-fix` |
| Clean up tech debt | `/harness:techdebt scan` -> `/harness:techdebt fix` |
| Review current changes | `/code-review` (quality) ‖ `/security` |
| Second opinion from another model | `claude-code ask --agent <leg> "<review brief>"` |

## Agent Reference

| Agent | Purpose | Phase |
|-------|---------|-------|
| `Explore` (built-in) | Codebase scanning | 2 |
| `Plan` (built-in) | Task vetting, plan-only architecture | 1, 4 |
| `general-purpose` (built-in) | Brief refinement, implementation, review | 5, 6 |
| `code-workers` | Parallel fan-out leaf — one scoped sub-task | 6 |
| `test-driven-dev` | New coverage, test-first | 6 |
| `test-repair` | Existing tests a change breaks — dispatched WITH the implementer | 6 |
| `claude-code ask --agent <leg>` | Cross-model review or an independent lane | 1, 6, 8 |

> **Security review is a skill, not an agent.** Run it via `/security` (the `security-reviewer`
> skill), typically alongside Phase 8.
