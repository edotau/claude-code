---
description: "Run a multi-stage pipeline for a task — explore, plan, (optional refine), implement, simplify, review, verify"
argument-hint: "[--refine] <task description>"
---

Run a multi-stage pipeline for a task.

Pipeline: `explore → plan → [refine] → implement → simplify → review → verify`.
The `refine` stage is **opt-in** — it runs only when `--refine` is passed.

Current branch:
```
!git branch --show-current
```

Existing worktrees:
```
!git worktree list 2>/dev/null
```

Stage handoff files:
```
!ls .claude/docs/plans/stages/*.md 2>/dev/null || echo "No active stage files"
```

Instructions:

1. Parse `$ARGUMENTS`. If the first token is `--refine`, set `INCLUDE_REFINE=true` and strip it;
   otherwise `INCLUDE_REFINE=false`. Derive a kebab-case task ID from the remaining description
   (e.g., "add user auth" → `add-user-auth`).

2. Create the stage handoff document `.claude/docs/plans/stages/{task-id}.md` (include
   `## Refined Briefs` ONLY when `INCLUDE_REFINE=true`):
   ```markdown
   # Task: {task-id}
   > **Description**: {task description}
   > **Created**: {date}
   > **Status**: in-progress
   > **Refine enabled**: {INCLUDE_REFINE}

   ## Findings
   ## Plan
   ## Refined Briefs
   ## Implementation Notes
   ## Simplification
   ## Review
   ## Verification
   ```

3. Run the stages sequentially. For each stage, dispatch an `Agent` (the subagent type in the table)
   whose prompt is: "Read `.claude/docs/plans/stages/{task-id}.md` and complete the {stage} section",
   plus the stage's own instructions. Writing stages (implement, simplify) scope each agent to
   disjoint files in the shared tree. **Skip `refine` entirely when `INCLUDE_REFINE=false`.**

4. Stages:
   | Stage | Agent | Notes |
   |-------|-------|-------|
   | explore | `Explore` | Structure, conventions, dependencies → `## Findings` |
   | plan | `Plan` | Task breakdown per the `planning` skill → `## Plan`; **stop for user approval** |
   | refine *(optional)* | `general-purpose` | Self-contained brief per sub-task → `## Refined Briefs` |
   | implement | `code-workers` per sub-task (parallel-safe ones in one message) **‖ `test-repair`** | Execute plan steps |
   | simplify | `general-purpose` loading the `simplicity` skill | Behavior-preserving cleanup |
   | review | `general-purpose` loading `evaluate-code` **‖** one loading `security-reviewer` | Findings + SHIP / FIX REQUIRED / BLOCK |
   | verify | main session, `approval-gate` skill | format → lint → test → security |

   For an independent second opinion at review, also run
   `claude-code ask --agent <leg> "<review brief with the diff range>"` from Bash (`claude-code agents`
   lists the available legs).

5. Update the stage document status to `complete`.

6. Print a summary: what was done, issues found in review, whether verification passed, and whether
   the refine stage ran (`refinement: ran` or `refinement: skipped (not requested)`).

The user can skip stages ("skip explore", "start from implement"). The stage handoff document is the
single source of truth for context between stages.
