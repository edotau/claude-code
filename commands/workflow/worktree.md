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

**Location** — one root shared by native Claude Code (the `worktree-create` hook), Orca, and this
command: `~/worktrees/<repo>/<slug>`, `<repo>` = the main checkout's basename. `create`, `list`,
`clean` and `clean-all` below delegate to `claude-code worktree …` (`internal/worktree`), which
resolves this root and every worktree's actual branch from git itself — never assume a naming
convention (`wt/<name>`, `worktree-<name>`, an Orca-chosen name): discover it. `merge` still
operates directly, since the merge/resolve-agent lifecycle is a later phase (Appendix A, Phase 2+).

---

## PARSE SUBCOMMAND

| Input | Action |
|-------|--------|
| `create <name> [--branch <b>]` | Create worktree + branch `worktree-<name>` (or an existing branch `<b>`) |
| `<name>` *(no subcommand)* | Same as `create <name>` |
| `list` | Show all worktrees with status |
| `merge <name> [--clean\|--no-clean]` | Preview commits, merge back, verify, optionally auto-clean |
| `clean <name>` | Remove a single worktree safely |
| `clean-all` | Remove all `~/worktrees/<repo>/*` worktrees |

If no arguments at all, run `list`.

---

## SUBCOMMAND: `create <name>`

Create a new worktree for a parallel Claude Code session — delegates the git plumbing to
`internal/worktree.Create`, the same function the `worktree-create` hook calls.

1. Derive a kebab-case slug from `<name>` (e.g., "add auth endpoint" -> `add-auth-endpoint`).

2. Create (or resume) the worktree. No `--branch` reuses `worktree-$SLUG` — the hook's naming,
   now shared by every creation path — if it already exists; resuming an existing `$SLUG`
   worktree is safe (it prints the same path rather than recreating):
   ```bash
   WT=$(claude-code worktree create "$SLUG" ${BRANCH:+--branch "$BRANCH"})
   ```
   A refusal here (bad name, traversal, or a foreign directory already occupying `$SLUG` — a
   basename collision) prints the reason and stops; do not retry with `-f` or similar.

3. Python repos only — symlink the shared `.venv` (avoids a 500MB+ copy):
   ```bash
   [ -d .venv ] && ln -sf "$(pwd)/.venv" "$WT/.venv"
   ```

4. Copy environment files:
   ```bash
   cp .env "$WT/.env" 2>/dev/null || true
   cp .claude/settings.local.json "$WT/.claude/settings.local.json" 2>/dev/null || true
   ```

5. Print: path, branch, base SHA, and the follow-ups — launch via `cd $WT && claude`, merge back via `/workflow:worktree merge $SLUG`, clean via
   `/workflow:worktree clean $SLUG`. Inside the harness repo (`~/.claude`) itself, the banned git verbs
   (hard rule 4) are allowed inside this worktree — it is a separate checkout — but `make install` and
   `claude-code` verbs there still target the live `~/.claude` unless `CLAUDE_CONFIG_DIR` points at it.

---

## SUBCOMMAND: `list`

Show all worktrees under `~/worktrees/<repo>/` with status — delegates to `internal/worktree.List`,
which unions git's own registration with the directories actually on disk and inspects each one live
(dirty/merged/stale/orphaned/prunable/locked), reading its real branch rather than assuming one:

```bash
claude-code worktree list
```

Each row already carries a quoted `cd` to reopen the worktree and, only when the live recheck
says it's eligible, the exact `claude-code worktree clean` command to remove it — present those
verbatim rather than re-deriving `$WT`/`$SLUG` by hand. If no worktrees exist under
`~/worktrees/<repo>/`, it prints that and stops.

---

## SUBCOMMAND: `merge <name> [--clean|--no-clean]`

Merge a worktree branch back into the current branch, then optionally clean up.

Flags:
- `--clean` — after a successful merge, auto-remove the worktree and delete the branch (no prompt).
- `--no-clean` — leave the worktree on disk (no prompt). This matches the pre-auto-clean behavior.
- *(no flag)* — after a successful merge, interactively ask `Clean up worktree $SLUG now? (Y/n)`.

1. Resolve `<name>` to the worktree path `$WT` and branch `wt/$SLUG`. Parse the optional `--clean` / `--no-clean` flag.

2. **Preview** — show what will be merged:
   ```bash
   git log --oneline HEAD..wt/$SLUG
   git diff --stat HEAD..wt/$SLUG
   ```

3. **Check for uncommitted changes** in the worktree:
   ```bash
   git -C $WT status --short
   ```
   If dirty, warn: "Worktree has uncommitted changes. Commit or stash them first."
   List the dirty files and stop. Do NOT merge with uncommitted changes. Cleanup is never reached on this path.

4. **Merge** with no-fast-forward to preserve branch history:
   ```bash
   git merge wt/$SLUG --no-ff -m "Merge wt/$SLUG into $(git branch --show-current)"
   ```
   If the merge exits non-zero (conflicts), skip to the **Merge conflict** block at the end — do NOT run Step 5, 6, or 7.
   This merge runs from the live checkout — it brings the worktree's commits in, never `checkout`/`reset`s the live tree to it.

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
         git -C $WT status --short
         ```
         If non-empty, print the "still on disk" reminder and stop (do not prompt — a dirty worktree is never silently cleaned).
         If empty, prompt: `Clean up worktree $SLUG now? (Y/n)`. On `n` / decline, print the "still on disk" reminder and stop.

   7b. Delegate to the same removal commands the `clean` subcommand uses:
       ```bash
       git worktree remove $WT --force
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

Safely remove a single worktree — delegates to `internal/worktree.Clean`, which rechecks clean,
merged, unlocked and owned state live immediately before removing (never trusts a prior `list`)
and never forces.

1. Preview first:
   ```bash
   claude-code worktree clean --dry-run "$SLUG"
   ```
   A refusal (uncommitted changes, unmerged HEAD, locked, or inspection failure) prints the exact
   reason — surface it and stop; do not retry with `--force` or similar. There is no such flag.

2. On a clean preview, ask for confirmation, then apply:
   ```bash
   claude-code worktree clean "$SLUG"
   ```
   This removes the worktree and, only if a plain `branch -d` succeeds (branch fully merged, no
   force), deletes the branch too — an unmerged branch is kept on purpose.

3. Print the command's own result line (`removed: …` / `branch deleted: …`, or `refused: … (reason)`).

---

## SUBCOMMAND: `clean-all`

Remove every eligible worktree under `~/worktrees/<repo>/` — delegates to
`internal/worktree.CleanAll`, which runs the same live recheck `clean` does, per worktree.

1. Preview:
   ```bash
   claude-code worktree clean-all --dry-run
   ```
   Prints every worktree as `eligible:` or `refused: … (reason)`, plus a summary count.

2. Present that summary to the user and ask: "Remove N worktrees? (Y/n)"

3. On confirmation, apply:
   ```bash
   claude-code worktree clean-all --yes
   ```

Unregistered directories under the root and git's own `prunable` administrative entries are
reported (by `list` and by `clean-all --dry-run`'s underlying listing) but never auto-removed in
this phase — surface them to the user, do not delete them.
