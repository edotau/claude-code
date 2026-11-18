---
paths:
  - "skills/subagent-workflows/**"
  - "agents/*.md"
  - "workflows/*.js"
---
# Master Workflow — End-to-End Development Pipeline

The pipeline for non-trivial work; dispatch follows `CLAUDE.md` guideline 3, brief and relay rules
`skills/subagent-workflows/SKILL.md`. Skip phases outside the requested scope, including commits for an edit-only
task. Phase 3 runs beside Phase 4 and joins at review.

```text
CLASSIFY → REVIEW → CODEBASE INTEL → RESEARCH → PLAN → [REFINE: opt-in] → DISPATCH → MERGE → VERIFY → COMMIT
```

## Scope tiers (Phase 0, inline)

| Scope | Signal | Pipeline |
|-------|--------|----------|
| TRIVIAL | Single-line fix, typo, rename | Inline — the only inline scope; skip all phases |
| LOW | One function or file | Dispatch (not inline); phases 2, 3, 8 |
| MEDIUM | 2–5 files or needs tests, one module | Phases 1–4, 8–9; fan out via 6 whatever is unblocked; no worktrees |
| HIGH | Cross-module, 5+ files | Full pipeline, parallel dispatch |
| EPIC | Multi-session, architectural | Full pipeline, phased across sessions |

`code-engineer`'s tiers size a job it was already asked to run; TRIVIAL with no workflow request still goes inline.

## Phases

| Phase | Agent / command | Output | Gate |
|-------|----------------|--------|------|
| 1. Review (MEDIUM+) | `Plan`, or a `codex` leaf | Refined Task Brief | READY / NEEDS CLARIFICATION |
| 2. Codebase intel | Scoped `Explore` leaves, or one `gemini` sweep | Codebase Context Brief | Structure, conventions, deps |
| 3. Research (new code) | `gh search`, `skills/web-explorer/SKILL.md`, or a `gemini` leaf | Prior art + library refs | ≥1 reference, or "nothing found" |
| 4. Plan (MEDIUM+) | plan mode or `Plan` | Plan with task breakdown | User approval |
| 5. Refine *(opt-in)* | `general-purpose` | Self-contained brief per sub-task | Cold-start test |
| 6. Dispatch | `skills/subagent-workflows/SKILL.md` | Completed sub-tasks | Each passes its own check |
| 7. Merge | `claude-code worktree merge` (if a worktree was used) | Merged diff | Clean, no leaf still running |
| 8. Verify | `rules/standards/quality.md` → Verification Order | Simplified, verified diff | Order green |
| 9. Commit | Authorized LAST-MILE `copilot` leaf or main session | Commit pushed | Clean tree, pushed |

- **1 Review** vets the request before code: checklist and verdicts `skills/evaluate-code/references/task-review.md`,
  agent choice `skills/subagent-workflows/references/cost-routing.md`. The same pass on a finished diff is
  `claude-code codex review [--adversarial] --base <branch>`, around Phase 8.
- **2 Intel** is three parallel `Explore` leaves (Structure, Conventions, Dependencies) merged into one brief for every
  downstream agent. Scope them to the directories the task touches: a repo-wide sweep is the biggest avoidable token
  cost. Skip if a brief exists this session and nothing changed.
- **3 Research** order: `gh search code|repos`, web search via `skills/web-explorer/SKILL.md`, package registries
  (prefer battle-tested libraries). Dispatch the research leaf (`gemini`, fallback `claude`) and `Plan` in one
  message, in the background; relay findings to the running `Plan` with `SendMessage`, or merge at review.
- **4 Plan** (rules `agents/Plan.md`, shape `docs/templates/plan.md`): review the draft before asking approval —
  in-context for MEDIUM, a `codex` leaf for HIGH+, per `skills/evaluate-code/references/task-review.md` → Plan review.
- **5 Refine**: one brief per sub-task with the fields in `skills/subagent-workflows/SKILL.md` → Handoff contract.
- **6 Dispatch**: shapes `skills/subagent-workflows/references/patterns.md`; a plan's `Phase W.k` waves run with
  `claude-code agents dispatch --waves <plan>` (never commits; `references/agent-verbs.md`).
- **7 Merge**: default is no worktree (`rules/standards/worktree-git.md`), else `skills/using-git-worktrees/SKILL.md`.
  Stop any leaf still running on merged work; a leaf at its turn limit reports partial work without committing.
- **8 Verify**: loop and caps `skills/problem-solving/references/verify.md`. Simplify runs once over the merged diff
  (parallel lanes duplicate helpers); before any new test, add nothing if tests already cover the change, delete dead
  code and stale tests it left, fold near-duplicate tests. **Never edit a migration to make a gate pass.**
- **9 Commit** only when authorized: `rules/standards/worktree-git.md` and `CLAUDE.md` hard rule 5; the main session
  owns release checks and push.

Tech debt: `skills/evaluate-code/SKILL.md` → Tech debt. Saved workflows (`workflows/*.js`) and `Workflow
code-looper` run only when the user asks: `skills/subagent-workflows/references/saved-workflows.md`.
