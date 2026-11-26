---
name: using-git-worktrees
description: >-
  Worktree commands, collection, landing, cleanup and shared-checkout coordination. Use for "git worktree",
  "parallel branches", "isolated workspace", "work on two branches" and "worktree cleanup".
---

# Using Git Worktrees

Tree selection and lifecycle: [Git and Worktrees](../../rules/standards/worktree-git.md#worktrees).

## Verbs

Check `claude-code worktree <verb> -h`. Flags precede names; `commit` also accepts interspersed flags.
Use the directory basename (`agent-<id>`), not the branch (`worktree-agent-<id>`).

| Verb | Use |
| --- | --- |
| `create <name>` | Create or reuse `~/worktrees/<repo>/<name>`; `--branch <b>` overrides `worktree-<name>`. |
| `list` | Inspect status, overlaps and cleanup commands; `--format json` for structured output. |
| `gate` | Inspect verification; `--dir`, `--scope`, `--lint-check`, `--format json`; `--run` executes. |
| `commit` | Named files, `-m <msg>`, `--task <id>`, `--plan <file>`; `--list` and `--dry-run` preview. |
| `collapse <collector> <lane>...` | Collect finished lanes; `--dry-run` previews. |
| `land <collector>` | Integrate and apply to local; `--verify <cmd>`, `--target <branch>`, `--dry-run`. |
| `clean <name>` | Remove an eligible worktree or kept branch; `--dry-run` previews; `--force` discards work. |
| `clean-all` | Preview eligible worktrees; `--yes` applies, `--dry-run` overrides it. |
| `base <ref>` / `base --clear` | Pin or clear the status-line diff base. |
| `pr`, `checks`, `comment` | Origin-host PR and CI operations; use each subcommand's help. |

`create` and native `EnterWorktree` share environment seeding: Python overlays redirect editable imports to lane
source, falling back to symlinks. `.env` is copied only to an absent, ignored destination. Existing and resumed
worktrees are not reseeded. Implementation: `internal/dev/worktree/{seed,venvoverlay}.go`.

## The lane protocol

Follow [Worktrees](../../rules/standards/worktree-git.md#worktrees) for creation, collection and commit authorization.

Current `collapse` cherry-picks lane commits and runs build/vet, not tests. It removes only a fully collected,
unchanged, clean, idle lane; inspect any retained lane before cleanup. A failed pick is rolled back in the collector.
Current `land` commits dirty lane files before integration. These mechanics do not implement uncommitted collection
under a no-commits hold; retain the diff for main rather than treating either verb as commit-free.

### Land: effects and recovery

Sign-off: `rules/standards/testing-guidelines.md` → Several worktrees: collapse, then test once.

`land` pins the primary branch and both heads, rebases in the collector and verifies the rebased tree. `--target`
requires the primary checkout's current branch. A preview neither runs verification nor proves landing will pass.
`--verify` overrides gate detection; detection details are in `internal/dev/worktree/gate.go`.

- Rebase conflict: resolve in the collector and retry; `--resolve-agent <agent>` delegates resolution,
  `--resume` reattaches to it.
- Moved target or busy collector: inspect the reported state before retrying; promotion rechecks heads and liveness.
- Apply failure: inspect local status before retrying; a failure after patch checking can leave partial writes.
- Dirty local overlap: inspect and resolve any conflict markers, preserving both sides' intent.
- Successful land: cleanup follows the Git rule; `--no-clean` keeps the worktree.

`clean` checks ownership, locks, liveness and whether the diff is in `HEAD`; ancestry alone is not required.
`clean --force` acts on an existing worktree, not a branch-only remainder, and still refuses main, locks or active use.
Discard authorization: `rules/standards/principles.md` → Communication.
Native `worktree-remove` uses safe branch deletion; it does not have `clean`'s landed-diff check.

### Hook enforcement vs. policy

Hooks cover their registered tools and events, not every filesystem write; see `internal/platform/settings/hookspec.go`.
Claim notices: [shared-checkout.md](references/shared-checkout.md). Dispatch and receipt checks:
`skills/subagent-workflows/references/hook-details.md`. Policy remains in the Git rule.

Manual creation and dev-server setup: [manual-worktrees.md](references/manual-worktrees.md).
