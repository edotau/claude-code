---
name: using-git-worktrees
description: Isolated git worktrees for parallel feature work, hotfixes, and plan execution — directory selection with gitignore safety checks, .venv symlink + .env copy setup, deterministic port allocation for parallel dev servers, and merged/stale cleanup. Triggers "git worktree", "parallel branches", "isolated workspace", "work on two branches", "worktree cleanup".
---

# Using Git Worktrees

Git worktrees give each branch its own working directory off one shared `.git`, so you can
run feature work, a hotfix, and a PR-review checkout side by side without stashing or
switching.

**Core principle:** systematic directory selection + gitignore safety + isolated ports =
reliable parallel sessions that never step on each other or pollute git status.

## When to use a worktree (vs. staying put)

| Situation | Action |
|-----------|--------|
| Need 2+ branches open locally at once | New worktree per branch |
| Hotfix needed while feature branch is dirty | Dedicated hotfix worktree (no stash) |
| Multiple agents/terminals, one branch each | One worktree per agent |
| Reproducing a bug on another branch | Temporary worktree, clean up same day |
| Quick local diff review only | Stay on current tree — no worktree |

## In this harness: use `/workflow:worktree` (the paved road)

The command handles everything with the harness conventions baked in:

```bash
/workflow:worktree create add-auth-endpoint   # → .claude/worktrees/add-auth-endpoint on branch wt/add-auth-endpoint
/workflow:worktree list                        # all worktrees + status
/workflow:worktree merge add-auth-endpoint     # preview commits, merge --no-ff, verify, optional clean
/workflow:worktree clean add-auth-endpoint     # safe single removal
```

It creates worktrees under `.claude/worktrees/<slug>` (gitignored), branches `wt/<slug>`,
**symlinks** the shared `.venv` (avoids a 500MB+ copy), and copies `.env` + `settings.local.json`.

## Manual control / non-harness repos

When `/workflow:worktree` doesn't apply, follow the full mechanics — directory selection,
gitignore safety, `.venv` symlink, port allocation for parallel dev servers, and gated
cleanup — in [references/manual-worktrees.md](references/manual-worktrees.md).

## Scoping a subagent below the root breaks git and the venv

A subagent or sandbox grant on a subdirectory instead of the repo root hides `.git` and
`.venv`, producing import errors that look like regressions — see
[references/grant-depth-trap.md](references/grant-depth-trap.md).
