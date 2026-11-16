# Git and Worktrees

1. **Conventional Commits** — `<type>[scope][!]: <description>`; types `feat` `fix` `docs` `style` `refactor` `perf`
   `test` `chore` `ci`; scope = package or area; `!` plus a `BREAKING CHANGE:` body line marks a break (MAJOR; `feat`
   MINOR, `fix` PATCH); `Fixes #42` where an issue exists. **1–2 lines total** — a body line only for what the diff
   cannot show. No AI-attribution footers (`safety-scan` blocks).
2. **Branches** — `feature/<domain>-<component>`, `fix/<issue>-<description>`, `docs/<component>`,
   `hotfix/<issue>-<description>`, `test/<feature>`; delete after merging.
3. **Stage named files only** — never a directory or a catch-all add (it sweeps `.env`/secrets and other sessions'
   work); confirm with the user before committing. A file another live session or leaf has uncommitted edits in asks
   first (`worktree-claim-check` blocks a leaf).
4. Atomic commits, one logical change each; never force-push a shared branch; never skip gates.
5. Read scoped checks' exit codes before `git push`, and chain lint → commit → push with `&&`, never `;`; a docs
   commit gates on a clean `make lint`.

Executor mechanics (account switching, the `git rm` staging gotcha): `agents/copilot.md` → LAST-MILE.

## Worktrees

**Default is the current branch.** Vault env `HARNESS_WORKTREE=off|false|0` (case-insensitive) means work on the
current branch with no asks; any other value, including unset, means ask the user once, before the first change,
whether to use a linked worktree, and again when another session joins the checkout. Never create one without the
user's yes; no answer means keep working in place.

- Never `git stash` in any worktree (the stash list is shared); rewrite history only in your own linked worktree
  (CLAUDE.md hard rule 4; `safety-scan` blocks). Coordination:
  `skills/using-git-worktrees/references/shared-checkout.md`.
- **A change you did not make is the owner's.** An uncommitted edit, new file or deletion you did not make in this task
  is the owner's or a peer's: never revert, overwrite, reformat or "clean up" it, even when it fails a gate; build on
  it and report what blocks you.
- A `notice (…)` or `local <branch> diverged` context line is from a peer or a merge: act on it before committing.
- Verbs, flags and refusals: `skills/using-git-worktrees/SKILL.md`.
