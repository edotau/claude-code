---
description: "Create and manage git worktrees for parallel Claude Code sessions"
argument-hint: "<create|list|merge|clean|clean-all> [name] [--branch <b>] [--clean|--no-clean]"
---

# worktree -- Git Worktree Lifecycle Manager

> Single source of truth for worktree creation, listing, merging, and cleanup.
> Used directly or delegated to by `/workflow:orchestrate`.
>
> **Native overlap**: `create`/`enter` overlap the built-in `EnterWorktree`/`ExitWorktree`
> tools — prefer those inside a session. This command's genuinely additive value is the
> **`merge`** and **`clean-all`** lifecycle (branch merge-back, bulk stale-worktree pruning)
> that the native tools don't cover. Fan-out Agent dispatches work in the shared tree on
> disjoint files rather than per-agent worktrees.

**Input**: $ARGUMENTS

Current branch:
```
!git branch --show-current
```

Existing worktrees:
```
!git worktree list 2>/dev/null
```

---

## PARSE SUBCOMMAND

| Input | Action |
|-------|--------|
| `create <name> [--branch <b>]` | Create worktree + branch `wt/<name>` (or an existing branch `<b>`) |
| `<name>` *(no subcommand)* | Same as `create <name>` |
| `list` | Show all worktrees with status |
| `merge <name> [--clean\|--no-clean]` | Preview commits, merge back, verify, optionally auto-clean |
| `clean <name>` | Remove a single worktree safely |
| `clean-all` | Remove all `.claude/worktrees/*` worktrees |

If no arguments at all, run `list`.

---

## SUBCOMMAND: `create <name>`

Create a new worktree for a parallel Claude Code session.

1. Derive a kebab-case slug from `<name>` (e.g., "add auth endpoint" -> `add-auth-endpoint`).

2. Check if worktree already exists:
   ```bash
   git worktree list | grep "wt/$SLUG"
   ```
   If it exists, warn and print the path. Do not recreate.

3. Create the worktree (branch `wt/$SLUG`, or the `--branch` override; reuse the branch if it
   already exists — `git worktree add .claude/worktrees/$SLUG $BRANCH` without `-b`):
   ```bash
   git worktree add .claude/worktrees/$SLUG -b ${BRANCH:-wt/$SLUG} HEAD
   ```

4. Python repos only — symlink the shared `.venv` (avoids a 500MB+ copy):
   ```bash
   [ -d .venv ] && ln -sf "$(pwd)/.venv" ".claude/worktrees/$SLUG/.venv"
   ```

5. Copy environment files:
   ```bash
   cp .env ".claude/worktrees/$SLUG/.env" 2>/dev/null || true
   cp .claude/settings.local.json ".claude/worktrees/$SLUG/.claude/settings.local.json" 2>/dev/null || true
   ```

6. Print: path, branch, base SHA, and the follow-ups — launch via `cd .claude/worktrees/$SLUG && claude`, merge back via `/workflow:worktree merge $SLUG`, clean via
   `/workflow:worktree clean $SLUG`.

---

## SUBCOMMAND: `list`

Show all worktrees under `.claude/worktrees/` with status.

1. Run:
   ```bash
   git worktree list
   ```

2. For each worktree under `.claude/worktrees/`, check:
   - Branch name
   - Ahead/behind vs parent branch: `git log --oneline HEAD..wt/$SLUG` (commits ahead)
   - Uncommitted changes: `git -C .claude/worktrees/$SLUG status --short`

3. Print a table: name, branch, commits ahead, dirty state.

If no worktrees exist under `.claude/worktrees/`, print "No active worktrees."

---

## SUBCOMMAND: `merge <name> [--clean|--no-clean]`

Merge a worktree branch back into the current branch, then optionally clean up.

Flags:
- `--clean` — after a successful merge, auto-remove the worktree and delete the branch (no prompt).
- `--no-clean` — leave the worktree on disk (no prompt). This matches the pre-auto-clean behavior.
- *(no flag)* — after a successful merge, interactively ask `Clean up worktree $SLUG now? (Y/n)`.

1. Resolve `<name>` to the worktree path `.claude/worktrees/$SLUG` and branch `wt/$SLUG`. Parse the optional `--clean` / `--no-clean` flag.

2. **Preview** — show what will be merged:
   ```bash
   git log --oneline HEAD..wt/$SLUG
   git diff --stat HEAD..wt/$SLUG
   ```

3. **Check for uncommitted changes** in the worktree:
   ```bash
   git -C .claude/worktrees/$SLUG status --short
   ```
   If dirty, warn: "Worktree has uncommitted changes. Commit or stash them first."
   List the dirty files and stop. Do NOT merge with uncommitted changes. Cleanup is never reached on this path.

4. **Merge** with no-fast-forward to preserve branch history:
   ```bash
   git merge wt/$SLUG --no-ff -m "Merge wt/$SLUG into $(git branch --show-current)"
   ```
   If the merge exits non-zero (conflicts), skip to the **Merge conflict** block at the end — do NOT run Step 5, 6, or 7.

5. **Verify** — run the project's fast test target after merge (discover it from the Makefile /
   `pyproject.toml` / `go.mod`), e.g.:
   ```bash
   make test-unit 2>&1 | tail -5   # or: go test ./... | tail -5 · .venv/bin/python -m pytest -x -q | tail -5
   ```
   Treat test-runner-not-found (no target, no `.venv`) as non-blocking — surface it in the report and continue. A real test failure is non-blocking for cleanup too, but is surfaced prominently so the user can decide.

6. **Report** (success path):
   ```
   Merge complete
   ==============
   Branch: wt/$SLUG -> <current branch>
   Commits merged: <N>
   Test result: <PASS/FAIL summary>
   ```

7. **Post-merge cleanup** — only reached when Step 4 succeeded.

   7a. Decide whether to clean:
       - If `--clean` was passed → proceed to 7b.
       - If `--no-clean` was passed → print the "still on disk" reminder (below) and stop.
       - Otherwise, re-check the worktree is still clean:
         ```bash
         git -C .claude/worktrees/$SLUG status --short
         ```
         If non-empty, print the "still on disk" reminder and stop (do not prompt — a dirty worktree is never silently cleaned).
         If empty, prompt: `Clean up worktree $SLUG now? (Y/n)`. On `n` / decline, print the "still on disk" reminder and stop.

   7b. Delegate to the same removal commands the `clean` subcommand uses:
       ```bash
       git worktree remove .claude/worktrees/$SLUG --force
       git branch -d wt/$SLUG 2>/dev/null || true
       ```
       Do NOT re-run the safety checks from `clean <name>` — Step 3 already verified the worktree was clean, and `--no-ff` preserves the branch's commits in history.

   7c. On success, print:
       ```
       Worktree $SLUG removed. Branch wt/$SLUG deleted.
       Commits are preserved on the merge commit (--no-ff).
       ```
       On removal failure (rare — e.g., filesystem lock), fall back to the "still on disk" reminder so the user can retry manually.

   **"still on disk" reminder** (printed whenever cleanup is skipped or declined):
   ```
   The worktree is still on disk. To clean up:
     /workflow:worktree clean $SLUG
   ```

---

### Merge conflict (Step 4 failure path)

If `git merge` exits non-zero, do NOT run verify or cleanup. Print:
```
Merge conflict
==============
Conflicting files:
  <list of files>

Resolve conflicts, then run:
  git add <resolved files>
  git commit

The worktree is still on disk. To clean up after resolving:
  /workflow:worktree clean $SLUG
```

---

## SUBCOMMAND: `clean <name>`

Safely remove a single worktree.

1. Resolve `<name>` to path `.claude/worktrees/$SLUG`.

2. **Check for unmerged commits**:
   ```bash
   git log --oneline HEAD..wt/$SLUG
   ```
   If there are unmerged commits, warn:
   ```
   WARNING: wt/$SLUG has N unmerged commit(s).
   Merge first with: /workflow:worktree merge $SLUG
   ```
   Ask for confirmation before proceeding.

3. **Check for uncommitted changes**:
   ```bash
   git -C .claude/worktrees/$SLUG status --short
   ```
   If dirty, warn about uncommitted files and ask for confirmation.

4. **Remove**:
   ```bash
   git worktree remove .claude/worktrees/$SLUG --force
   git branch -d wt/$SLUG 2>/dev/null || true
   ```

5. Print: "Worktree `$SLUG` removed. Branch `wt/$SLUG` deleted."

---

## SUBCOMMAND: `clean-all`

Remove all worktrees under `.claude/worktrees/`.

1. List all worktrees under `.claude/worktrees/`:
   ```bash
   git worktree list | grep '.claude/worktrees/'
   ```

2. For each, check for unmerged commits and uncommitted changes (same as `clean`).

3. Print a summary of what will be removed, including warnings for any with unmerged work.

4. Ask for confirmation: "Remove N worktrees? (Y/n)"

5. Remove each:
   ```bash
   git worktree remove .claude/worktrees/$SLUG --force
   git branch -d wt/$SLUG 2>/dev/null || true
   ```

6. Final cleanup — prune stale worktree references:
   ```bash
   git worktree prune
   ```

7. Print: "Removed N worktrees. Run `git worktree list` to verify."
