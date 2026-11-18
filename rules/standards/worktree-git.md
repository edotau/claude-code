# Git and Worktrees

1. **Conventional Commits** — `<type>[scope][!]: <description>` (types `feat` `fix` `docs` `style` `refactor` `perf`
   `test` `chore` `ci` `build` `revert`), a subject over 150 characters warns, 2 lines max plus a `BREAKING CHANGE:`
   line, no AI-attribution footers (`safety-scan` blocks);
   `Fixes #42` where an issue exists; a body line only for what the diff cannot show.
2. **Branches** — `feature/<domain>-<component>`, `fix/<issue>-<description>`, `docs/<component>`,
   `hotfix/<issue>-<description>`, `test/<feature>`; delete after merging.
3. **Stage named files only** (never `add -A`, `.` or a directory); commit when the request or
   an approved wave plan authorizes it, else confirm first.
   `claude-code worktree commit --task <id> -m <msg> [files]` is the way
   to commit: it takes only named files, or with none the main session's own claims (a subagent or CLI leaf must name
   its files; a CLI leaf is refused any claimed file), and refuses a peer's.
   Repeat `--task` for multiple tasks; `--plan <file>` selects the plan when ids collide. Each id must name one open
   task (a recent or claimed plan, or a dated `--plan` file in the repo's plans home) before staging; quote an id with
   spaces. The commit carries `Task: <plan-slug> <id>` trailers: only a real trailer counts (last paragraph, every line
   a column-zero trailer; quoted or prose lines never do); a named task ticks once the commit is on main (a lane
   commit when it lands), even if it left some `owns:` files out. Main is the primary checkout on the trunk: origin/HEAD
   when it names an existing local branch, else `init.defaultBranch`, `main`, `master`; none exists, nothing ticks.
   Coverage alone prints one candidate and changes nothing. Manual
   `plan task <id> --done --sha <sha> --plan <file>` needs no trailer.
   A file another live session or leaf has
   uncommitted edits in asks first (`worktree-claim-check` blocks a leaf under `~/.claude`, asks elsewhere).
4. Atomic commits, one logical change each; never force-push a shared branch; never skip gates.
5. Read scoped checks' exit codes before `git push`; chain with `&&`, never `;`; a docs
   commit gates on a clean `claude-code agents diff --check`.

Executor mechanics: `agents/copilot.md` → LAST-MILE.

## Worktrees

**Default is the current branch.** Vault env `HARNESS_WORKTREE=off|false|0` (case-insensitive) means work on the
current branch with no asks; any other value, including unset, means ask the user once, before the first change,
whether to use a linked worktree, and again when another session joins the checkout. Never create one without the
user's yes; no answer means keep working in place.

`HARNESS_WORKTREE=lanes` asks nothing: the claim check refuses (a leaf) or asks (main) an edit or stage of a tracked
file in the live checkout and names `claude-code worktree create <slug>`; a lane is never refused.

- **Lane protocol:** one lane per stream; edit and `worktree commit` in the lane; `worktree merge <lane>` rebases the
  lane onto `main` inside the lane, gates the rebased tree and fast-forwards `main` (a moved `main` is refused: re-run).
  A conflict exits 3, the lane stays as it was and the files are named: fix it in the lane, then re-run the merge.
  Never rebase or reset `main`. The full audit runs on clean `main`, main session only.

- Never `git stash` in any worktree (the stash list is shared; convention in a linked worktree); rewrite history only in
  your own linked worktree (CLAUDE.md hard rule 4). `safety-scan` blocks stash|checkout|switch|reset|restore|clean under
  `~/.claude` (a new branch, an unstage and dry runs pass) and `commit --amend`, `rebase` and `reset <commit>` in a
  shared checkout (a leaf outside `~/.claude` is held to the main session's rules). Coordination:
  `skills/using-git-worktrees/references/shared-checkout.md`.
- **A change you did not make is the owner's.** An uncommitted edit, new file or deletion you did not make in this task
  is the owner's or a peer's: never revert, overwrite, reformat or "clean up" it, even when it fails a gate; build on
  it and report what blocks you.
- A `notice (…)` or `local <branch> diverged` context line is from a peer or a merge: act on it before committing.
- Verbs, flags and refusals: `skills/using-git-worktrees/SKILL.md`.
