# Git and Worktrees

1. **Conventional Commits** — `<type>[scope][!]: <description>`; types `feat` `fix` `docs` `style` `refactor` `perf`
   `test` `chore` `ci`; scope = package or area; `!` + a `BREAKING CHANGE:` body line marks a break (→ MAJOR;
   `feat` → MINOR, `fix` → PATCH); `Fixes #42` where an issue exists. **1–2 lines total** — a body line only for
   what the diff cannot show, never a change inventory.
2. **Branches** — `feature/<domain>-<component>`, `fix/<issue>-<description>`, `docs/<component>`,
   `hotfix/<issue>-<description>`, `test/<feature>`; delete after merging.
3. **Stage named files only — never `git add -A` or a directory** (sweeps `.env`/secrets and other sessions'
   work); confirm with the user before committing.
4. Atomic commits, one logical change each; never force-push a shared branch; never skip gates.

```bash
feat(router): fail over on 529 before the first byte
fix(providers): expand ~ in auth.file
feat(providers)!: rename auth.cmd to auth.command   # + BREAKING CHANGE: body line
```

Executor mechanics (account switching, the `git rm` staging gotcha): `agents/copilot.md` → LAST-MILE.

## Worktrees

**Default is the current branch.** Vault env `HARNESS_WORKTREE=off` means work on the current branch with no asks;
unset (or any other value) means ask the user once, before the first change, whether to use a linked worktree, plus
when another session joins the checkout. Never create one without the user's yes; no answer means keep working in place.

| To… | Run |
| --- | --- |
| List with status and cleanup commands | `claude-code worktree list [--format json]` |
| Create on branch `worktree-<name>` | `claude-code worktree create <name>`, or `EnterWorktree` to move the session in |
| Merge back (verified fast-forward) | `claude-code worktree merge <name> [--dry-run\|--full-verify]` |
| Remove merged ones | `claude-code worktree clean <name>` · `clean-all [--dry-run\|--yes]` |

- Never `stash`/`checkout`/`reset`/`restore` the live checkout (CLAUDE.md hard rule 4); in a worktree, branch freely.
- No `git stash` anywhere — the stash list is shared by every worktree. Never run a worktree-built binary as the proxy.
- `make install` and `claude-code` verbs still write to `~/.claude` from a worktree unless `CLAUDE_CONFIG_DIR` points
  at it (`rules/harness/settings-management.md` → Worktree render).
- `merge` refuses when `main` moved mid-verify — re-run it; `clean` refuses while a process has its cwd inside —
  `cd` out (or `ExitWorktree`) first.
- Conflicts, the verify gate and manual worktrees: `skills/using-git-worktrees/references/merge-and-clean.md`.
