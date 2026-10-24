# Harness worktrees: create, list, merge, clean

The one procedure behind `claude-code worktree` and the `worktree-create`/`worktree-remove` hooks. Every verb
delegates to `claude-code worktree …` (`internal/worktree`), which resolves the root and each worktree's real
branch from git — never assume a naming convention; discover it. Root: `~/worktrees/<repo>/<slug>`, shared
with native Claude Code and Orca. Manual mechanics for non-harness repos: [manual-worktrees.md](manual-worktrees.md).

## The merge rule (from `internal/worktree/merge.go`)

Promotion is **fast-forward only**: `Preflight` pins both OIDs and simulates with `git merge-tree`,
`IntegrateTarget` merges the pinned target **inside the worktree** and runs the verify gate there, `Promote`
runs `git merge --ff-only` on the main checkout and refuses any OID other than the one just verified. Never a
`--no-ff` merge commit on the live branch, never a real conflicting merge there. A predicted conflict stops
before anything mutates unless `--resolve-agent claude|codex` hands it to an agent inside the worktree.

## create

```bash
WT=$(claude-code worktree create "$SLUG" ${BRANCH:+--branch "$BRANCH"})   # resumes an existing slug safely
[ -d .venv ] && ln -sf "$(pwd)/.venv" "$WT/.venv"                          # Python repos: no 500 MB copy
cp .env "$WT/.env" 2>/dev/null; cp .claude/settings.local.json "$WT/.claude/settings.local.json" 2>/dev/null
```

A refusal (bad name, traversal, a foreign directory on `$SLUG`) prints the reason; never retry with a force
flag. Print path, branch, base SHA; launch with `cd $WT && claude`. Inside `~/.claude` the worktree is a
separate checkout exempt from hard rule 4, but `make install` and `claude-code` verbs still target the live
tree unless `CLAUDE_CONFIG_DIR` points at it.

## list

`claude-code worktree list` unions git's registration with the directories on disk and inspects each live
(dirty / merged / stale / orphaned / prunable / locked). Present its `cd` and `clean` lines verbatim.

## merge

1. **Preview, always:** `claude-code worktree merge --dry-run "$SLUG"`. `clean — no predicted conflicts`
   exits 0; any `issue:` (dirty checkout, detached target, `--target` mismatch, unrelated histories,
   merge-tree failure, predicted `conflict [<files>]`) exits 1 with nothing mutated — report it and stop.
2. Ask: "Merge $SLUG into <target>? (Y/n)".
3. **Apply:** `claude-code worktree merge "$SLUG"` (`--verify "<cmd>"` overrides auto-detection from
   Makefile test / go.mod / pyproject / package.json / Cargo.toml; `--no-verify` is the only unverified path;
   no gate found refuses before integrating). Preflight re-runs; a failing gate stops with the worktree
   untouched on disk; success promotes and, unless `--no-clean`, cleans exactly as `clean` would. A failure
   after promotion prints a retry hint instead of leaving a merge commit; re-run step 1.
4. **Report:** `Merge complete` / `Branch: <branch> -> <target>` / `Verify: <cmd> pass|fail`.

**Predicted conflict:** `claude-code worktree merge --resolve-agent codex "$SLUG"` materializes it inside the
worktree, runs the agent once on the conflicted paths only (no `git add`/`commit` by the agent), then the
controller checks every marker is gone and nothing else changed, commits, revalidates ancestry, and continues
to promote. Markers left, an unexpected path, a silent codex→Claude fallback, a timeout or Ctrl-C fail the
operation with everything left on disk. `claude-code worktree merge --resume "$SLUG"` reattaches after a
crash without re-invoking the agent.

## clean / clean-all

`claude-code worktree clean --dry-run "$SLUG"`, confirm with the user, then `clean "$SLUG"`: it rechecks clean,
merged, unlocked and owned state live and never forces (there is no force flag); the branch is deleted only
when a plain `branch -d` succeeds. `clean-all --dry-run` lists `eligible:` / `refused: … (reason)`; confirm
"Remove N worktrees?", then `clean-all --yes`. Unregistered directories and git's `prunable` entries are
reported, never auto-removed.
