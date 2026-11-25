# Git and Worktrees

1. **Conventional Commits** — `<type>[scope][!]: <description>` (types `feat` `fix` `docs` `style` `refactor` `perf`
   `test` `chore` `ci` `build` `revert`), a subject over 150 characters warns, 2 lines max plus a `BREAKING CHANGE:`
   line, no AI-attribution footers in a commit, merge, tag or PR (`safety-scan` blocks them);
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
   a column-zero trailer; quoted or prose lines never do); a named task ticks once the commit is on main and its
   `verify:` plus the scoped gate pass on that commit (detached; the result is a notice) (a lane commit once you
   commit its landed changes on main), even if it left some `owns:` files out. Main is the primary checkout on the
   trunk: origin/HEAD when it names an existing local branch, else `init.defaultBranch`, `main`, `master`; none
   exists, nothing ticks. Only the main session's `worktree commit --task`, after it verifies a hand-back, ticks
   (leaves hand back uncommitted); coverage never ticks, it prints one candidate. Manual
   `plan task <id> --done --sha <sha> --plan <file>` needs no trailer.
   Plans finishing together run their closing steps once and tick each plan's lines with that sha; strike only a
   step really dropped.
   A file another live session or leaf has
   uncommitted edits in asks first (`worktree-claim-check` blocks a leaf under `~/.claude`, asks elsewhere).
4. Atomic commits, one logical change each; never force-push a shared branch; never skip gates.
5. Read scoped checks' exit codes before `git push`; chain with `&&`, never `;`; a docs
   commit gates on a clean `claude-code agents diff --check`.

Executor mechanics: [LAST-MILE](#last-mile).

## LAST-MILE

Any executor given an authorized landing brief lands already-verified work; inspect the tree before resuming.

1. Land verified branches in dependency order with `claude-code worktree land <name>` (`--dry-run` previews;
   `--verify <cmd>` sets the gate). Return conflicts needing code decisions to the caller; never change product code.
2. After the final land, run lint, build and tests of the merged packages once: individually green branches can fail
   together. The release/full build, test and security chain remains main-session-only, once, with owner approval.
3. Separate your changes from pre-existing dirt. Inspect `rtk git diff --no-ext-diff --no-textconv HEAD -- <paths>`
   and scan the staged diff for secrets. Leave every unrelated index entry untouched and report it under `excluded`.
4. Use the named-file commit command above (`--dry-run` previews the set); working-tree content is what lands.
   Use the user's message, otherwise confirm a Conventional message first. Verify the selected content builds and
   tests independently where supported, in an isolated snapshot under the shared-checkout rules below.
5. Check `rtk git log --no-ext-diff --no-textconv -1 --format=%B` before reporting AI-attribution `none (verified)`.
   Report the commit hash and resulting tree state.

Lead with `DONE | DONE_WITH_CONCERNS | NEEDS_CONTEXT | BLOCKED`, scoped gate results, `excluded` and blockers.
For a dispatched job, keep its actual `ask: ran on …` receipt and job/session ids, then the verdict under `## Result`
(`skills/subagent-workflows/SKILL.md` → External relay contract); never invent a receipt or verb.
Do not write memory; propose it in the report. Never force-push shared history or rewrite published commits (rule 4).
Test repair, including the prohibitions on deleting tests or inventing a framework: `testing-guidelines.md` → TEST.

## Worktrees

**Default is the current branch.** Vault env `HARNESS_WORKTREE=off|false|0` (case-insensitive) means work on the
current branch with no asks; any other value, including unset, means ask the user once, before the first change,
whether to use a linked worktree, and again when another session joins the checkout. Never create one without the
user's yes; no answer means keep working in place.

`HARNESS_WORKTREE=lanes` asks nothing and blocks nothing: an edit or stage of a tracked file in the live checkout, or in
another repo's primary checkout, gets a note naming `claude-code worktree create <slug>`; work in a lane gets none.

- **Landing:** every repo, `~/.claude` included, lands on local, the only way a lane comes back: the gate-green lane's
  net diff becomes uncommitted changes in the live checkout; you commit (owner, 2026-11-24: "no merging, just land on
  local"). A verified lane lands without asking, with a summary plus `git diff --stat main...<lane>`, unless its diff
  adds a restriction (block, deny, refusal, narrower sandbox): that waits for the owner's yes.
- **Lane protocol:** one lane per stream; edit and `worktree commit` in the lane; `worktree land <lane>` rebases the
  lane onto `main` inside the lane, gates it (build and vet everywhere; tests of changed packages and their direct
  importers; all concurrent) and applies its net diff to the live working tree, uncommitted and unstaged. A landed
  path already dirty in the live tree, or a patch that does not check, changes nothing, keeps the lane and exits 3
  naming the paths (a write failure after a clean check exits 1, may be partial, and keeps the lane; a change you
  did not make is never overwritten). A clean land removes the worktree, keeps the branch (the only committed copy
  until you commit; a detached lane keeps its worktree) and prints `landed <lane>: N files uncommitted in <dir>;
  branch <b> kept` plus the diff stat. `worktree clean <lane>` deletes the kept branch once its diff is in `HEAD`.
  Plan ticks fire when you commit with `Task:` trailers or run `plan task --done`, not at landing. The full
  `rtk make audit` runs detached on a snapshot of the lane's rebased head (`HEAD` plus the landed diff), reported on
  your next prompt.
  A rebase conflict exits 3 too, the lane stays as it was and the files are named: fix it in the lane, then re-run.
  Never rebase or reset `main`. The full audit runs on clean `main`, main session only.
- **Land on completion (every fan-out worktree, any `HARNESS_WORKTREE` value):** the moment a task's leaf hands back
  and its scoped check passes, the main session commits the named files in the worktree and runs
  `claude-code worktree land --resolve-agent <agent> <lane>` (flags before the name) onto the live checkout,
  without asking and without waiting for sibling tasks. A conflict is resolved in the worktree, keeping both sides'
  intent, then the land is re-run (`--resume`); it never goes back to the owner unless the two sides contradict
  an owner statement. A landed worktree is removed by the land; one it refuses to remove (a process still inside
  it) is removed with `claude-code worktree clean <lane>` once that process ends. No finished worktree outlives
  its task. A lane that cannot land by itself is queued, and the main session works the queue off at once.
  Several lanes finished at the same time are collapsed into one first and gated once
  (`testing-guidelines.md` → Several worktrees: collapse, then test once).
  Enforced by `lane-land` (`rules/harness/hooks.md`).

- Never `git stash` in any worktree (the stash list is shared; convention in a linked worktree); rewrite history only in
  your own linked worktree (CLAUDE.md hard rule 4). Never run stash, checkout, switch, reset, restore or clean in
  `~/.claude`, nor `commit --amend`, `rebase` or `reset <commit>` in a shared checkout; a new branch, an unstage and
  dry runs are fine. Coordination: `skills/using-git-worktrees/references/shared-checkout.md`.
- **A change you did not make is the owner's.** An uncommitted edit, new file or deletion you did not make in this task
  is the owner's or a peer's: never revert, overwrite, reformat or "clean up" it, even when it fails a gate; build on
  it and report what blocks you.
- A `notice (…)` or `local <branch> diverged` context line is from a peer or a merge: act on it before committing.
- Verbs, flags and refusals: `skills/using-git-worktrees/SKILL.md`.
