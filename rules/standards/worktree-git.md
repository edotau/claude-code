# Git and Worktrees

1. **Conventional Commits** — `<type>[scope][!]: <description>`; types: `feat`, `fix`, `docs`, `style`, `refactor`,
   `perf`, `test`, `chore`, `ci`, `build`, `revert`. A subject over 150 characters warns; 2 lines max plus a
   `BREAKING CHANGE:` line. Use `Fixes #42` where an issue exists; a body line only for what the diff cannot show.
   No AI-attribution footers in commits, merges, tags or PRs (`safety-scan` blocks them).
2. **Branches** — `feature/<domain>-<component>`, `fix/<issue>-<description>`, `docs/<component>`,
   `hotfix/<issue>-<description>`, `test/<feature>`; delete after merging, subject to Worktrees' cleanup rule below.
3. **Stage named files only** — never `git add -A`, `git add .` or a directory. Commit with
   `claude-code worktree commit --task <id> -m <msg> <files>`, explicitly naming every file. Commit only when the
   request or approved wave plan authorizes it; otherwise confirm first.
   Plan completion: `standing-requirements.md` → R1.
   **An owner's "no commits" means nothing commits anywhere, lanes included; hand back and land uncommitted.**
4. **Atomic commits:** one logical change each. Never force-push a shared branch, rewrite published commits or skip
   gates. Read scoped checks' exit codes before pushing; chain with `&&`, never `;`. A docs commit requires a clean
   `claude-code agents diff --check`. Push only as directed by `CLAUDE.md` hard rule 5.

## LAST-MILE

Stop at the requested boundary: an edit or review is not authorization to land or commit (`CLAUDE.md` hard rule 5).
For an authorized landing, inspect the tree before resuming and follow Worktrees below; testing belongs only at
`testing-guidelines.md` → Several worktrees: collapse, then test once, not in a second post-landing gate chain.

Before committing, separate your changes from pre-existing dirt and inspect the selected and staged diffs for
secrets. Leave unrelated index entries untouched and report them under `excluded`. Use the owner's commit message;
otherwise confirm a Conventional message first. Inspect the actual commit message before reporting no attribution.
Report status, scoped gate results, `excluded`, blockers, resulting tree state and the commit hash if one was made.

## Worktrees

**Default is the current branch, with no worktree.** Worktrees exist only for parallel subagents. Vault env
`WORKTREE_MODE=off|false|0` (case-insensitive) always uses the current branch; `lanes` uses lanes for parallel work
without asking; unset means ask the owner once before the first change whether parallel work may use lanes.
With no parallel work, the live checkout is the only tree and there is no worktree, including in `lanes` mode.
Command flags, seeding and recovery: `skills/using-git-worktrees/SKILL.md`.

- **Topology:** `n = 1` creates one collector (`claude-code worktree create <name>` from the working branch); `n > 1`
  creates one collector plus one lane per parallel subagent from the collector's head, never from `main`.
- **Lane hand-back:** each subagent edits only its owned files and runs only cheap checks (`testing-guidelines.md`).
  When commits are authorized, Claude leaves commit named files in-lane (`worktree commit --task <id> -m <msg>
  <files>`); relay types (`codex`, `copilot`, `gemini`, `opencode`, `coworker`) hand back uncommitted for main to commit
  in-lane. An owner's "no commits" overrides both: hand back and land uncommitted.
- **Collect, land and clean:** as each lane finishes, main runs `worktree collapse <collector> <lane>` (no per-lane
  review or tests; resolve conflicts keeping both intents). Once collected, follow `testing-guidelines.md` → Several
  worktrees: collapse, then test once; `worktree land <collector>` rebases, gates in the collector, and applies the
  net diff uncommitted to the live checkout (a diff adding a restriction waits for the owner's yes). After the diff is
  in `HEAD`, run `worktree clean <name>`; no finished worktree outlives its task.

Never `git stash` in any worktree: the stash list is shared. In `~/.claude`, never stash, checkout, switch, reset,
restore, clean, rebase or amend a commit. Never amend, rebase or reset commits in a shared checkout; rewrite history
only in your own linked worktree. Never reset or rebase `main`. A new branch, unstaging named files and dry runs are
fine. Coordination: `skills/using-git-worktrees/references/shared-checkout.md`.

**A change you did not make is the owner's.** An edit, new file or deletion you did not make in this task belongs
to the owner or a peer: never revert, overwrite, reformat or "clean up" it, even when it fails a gate. Build on it
and report blockers. A peer's edits or claims get a notice, never a staging or commit refusal. Act on peer or merge
context (`notice (…)`, `local <branch> diverged`) before committing.
