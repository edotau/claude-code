---
description: "Run a multi-stage pipeline for a task — explore, plan, (optional refine), implement, simplify, review, verify"
allowed-tools: Bash(git branch *) Bash(git worktree *) Bash(ls *)
argument-hint: "[--scan | --refine] <task description>"
---

Current branch: !`git branch --show-current`

Existing worktrees: !`git worktree list 2>/dev/null`

Stage handoff files: !`ls ~/.claude/plans/stages/*.md 2>/dev/null || echo "No active stage files"`

Read `skills/subagent-workflows/references/orchestrate.md` and run it for: $ARGUMENTS
