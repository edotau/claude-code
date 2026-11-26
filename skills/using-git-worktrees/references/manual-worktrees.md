# Manual Worktree Mechanics

Harness verbs, seeding, landing and refusals: [the skill](../SKILL.md#verbs).
This reference is for repositories where those verbs do not apply. The recommendations below are **prose**, not
harness hook enforcement; Git's own checks are named where relevant. Authorization and shared-history policy:
[Git and Worktrees](../../../rules/standards/worktree-git.md#worktrees).

## Create outside the checkout

Choose the project's documented directory; prefer an external sibling such as `~/worktrees/<repo>/<slug>`.
A project-local `.worktrees` alternative must be ignored first (`git check-ignore -q .worktrees`); an external
parent needs no ignore rule. Do not copy the main tree's local permission overrides blindly.

From the collector for a sibling lane, or the local working branch for the collector, choose an unused slug:

```bash
slug=add-auth-endpoint
parent=$PWD
repo=$(basename "$parent")
wt="$HOME/worktrees/$repo/$slug"
rtk git worktree add -b "wt/$slug" "$wt" HEAD && cd "$wt"
```

Git refuses an existing branch with `-b` or a branch already checked out elsewhere.
Grant a sandbox the worktree root rather than only a source subdirectory so `.git` and the environment stay visible.

## Environment and baseline

Reuse dependencies only when compatible. A plain `.venv` symlink can still import the **primary** checkout's
editable source; the harness overlay avoids that when it can. For manual lanes, check imports resolve to lane
source or create a lane-local environment. Copy `.env` only to an absent, ignored destination; never commit secrets.
Check scope: `rules/standards/testing-guidelines.md` → Several worktrees: collapse, then test once.

## Parallel dev servers

Allocate a unique port block per lane, with stride at least the number of services (for example app/db/redis:
8010/5442/6389, then 8020/5452/6399). Record the allocation in an ignored local file and pass it to the dev command
or compose override. Check actual listeners too. Never feed these overrides into production configuration.

## Cleanup

Use the values captured at creation (re-set them to the actual paths/name if the shell restarted):

```bash
: "${parent:?set primary checkout root}" "${wt:?set worktree path}" "${slug:?set lane slug}"
cd "$parent" &&
rtk bypass git -C "$wt" status --porcelain &&
rtk git branch --merged HEAD --list "wt/$slug"
```

Read both outputs and check running processes before removal. Empty branch output means it is not merged;
confirm integration before continuing:

```bash
rtk git worktree remove "$wt" && rtk git branch -d "wt/$slug"
rtk git worktree prune --dry-run
```

An empty status alone is not proof the work was integrated. Git normally refuses dirty removal and `branch -d`
refuses an unmerged branch; plain Git does **not** provide the harness's process-cwd check. Intentional discard
needs explicit approval under the standard. Use `git worktree prune` only after inspecting its preview; age alone
is not proof a lane is disposable. Prefer `git worktree remove` to deleting directories by hand.
