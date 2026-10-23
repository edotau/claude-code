---
description: "Create and manage git worktrees for parallel Claude Code sessions"
allowed-tools: Bash(git branch *) Bash(git worktree *) Bash(claude-code worktree *)
argument-hint: "<create|list|merge|clean|clean-all> [name] [--branch <b>] [--target <ref>] [--dry-run|--no-clean] [--verify \"<cmd>\"|--no-verify] [--resolve-agent claude|codex|--resume]"
---

# worktree — launcher for `claude-code worktree <verb>`

**Input**: $ARGUMENTS (no arguments → `list`; a bare `<name>` → `create <name>`).

Procedure, the merge rule and the confirmation points live once in
`skills/using-git-worktrees/references/merge-and-clean.md`; read it, then run the matching verb:

| Input | Runs |
|-------|------|
| `create <name> [--branch <b>]` | `claude-code worktree create <slug> [--branch <b>]`, then the venv/env steps |
| `list` | `claude-code worktree list` |
| `merge <name> [--target <ref>] [--no-clean] [--verify "<cmd>"\|--no-verify]` | `--dry-run` preview, confirm, apply |
| `merge <name> --resolve-agent claude\|codex` · `--resume` | agent resolution of a predicted conflict; reattach |
| `clean <name>` · `clean-all` | `--dry-run` preview, confirm, apply (`clean-all --yes`) |

Inside a session prefer the native `EnterWorktree`/`ExitWorktree` for create/enter; this launcher's additive
value is `merge` and `clean-all`. Never pass a force flag: every verb refuses with a reason instead.
