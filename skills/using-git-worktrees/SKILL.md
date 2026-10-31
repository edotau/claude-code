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
| Human terminal or long-running background agent owning its own branch | One worktree per agent |
| Parallel fan-out leaves in one session | The orchestration skill decides (often one shared tree) |
| Reproducing a bug on another branch | Temporary worktree, clean up same day |
| Quick local diff review only | Stay on current tree — no worktree |

## In this harness: use `claude-code worktree` (the paved road)

The verbs handle everything with the harness conventions baked in:

```bash
claude-code worktree create add-auth-endpoint   # → ~/worktrees/<repo>/add-auth-endpoint, branch worktree-add-auth-endpoint
claude-code worktree list [--format json]        # all worktrees + status
claude-code worktree merge add-auth-endpoint     # preview, verify gate, fast-forward-only promote, clean
claude-code worktree clean add-auth-endpoint     # safe single removal
```

More verbs and options:

| Verb | Does |
|------|------|
| `claude-code worktree create <name> [--branch <b>]` | New worktree, then the venv/env steps |
| `claude-code worktree merge <name> [--target <ref>] [--no-clean] [--verify "<cmd>"\|--no-verify\|--full-verify]` | `--dry-run` preview, confirm, apply |
| `claude-code worktree merge <name> --resolve-agent claude\|codex` · `--resume` | Agent resolves a predicted conflict; reattach |
| `claude-code worktree clean <name>` · `clean-all [--yes]` | `--dry-run` preview, confirm, apply |

`clean`/`clean-all` take `--force` (discard uncommitted and unmerged work); every other verb refuses
with a reason instead. Only pass `--force` with the user's explicit go — it is a data-loss operation.
`merge` refuses when `main` moved mid-verify (re-run it); `clean` refuses while a process has its cwd
inside (`cd` out or `ExitWorktree`).
Inside a session prefer the native `EnterWorktree`/`ExitWorktree` for create/enter; the additive value
here is `merge` and `clean-all`.

`create` makes the worktree under `~/worktrees/<repo>/<slug>` (outside the repo, shared with native
Claude Code and Orca) via `git worktree add`; the `.venv` symlink and `.env` copy are manual follow-up
lines, not something `create` does itself. The full procedure, those follow-up lines, the
fast-forward-only merge rule and conflict handling:
[references/manual-worktrees.md](references/manual-worktrees.md) Part 1.
In the harness repo itself, a worktree here is a separate checkout exempt from CLAUDE.md hard rule 4 —
but `make install` and `claude-code` verbs run from it still target the live `~/.claude`.

## Manual control / non-harness repos

When `claude-code worktree` doesn't apply, follow the full mechanics (Part 2) — directory selection,
gitignore safety, `.venv` symlink, port allocation for parallel dev servers, and gated
cleanup — in [references/manual-worktrees.md](references/manual-worktrees.md).

## Scoping a subagent below the root breaks git and the venv

A subagent or sandbox grant on a subdirectory instead of the repo root hides `.git` and
`.venv`, producing import errors that look like regressions — see `skills/subagent-workflows/SKILL.md` → the
scoping note under the reference list (a dispatch concern, not a worktree one).
