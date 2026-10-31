# Worktree Mechanics: harness verbs and manual control

Part 1 is the procedure behind `claude-code worktree` and the `worktree-create`/`worktree-remove` hooks
(`internal/worktree`); every verb resolves the root and each worktree's real branch from git — never assume a
naming convention. Root: `~/worktrees/<repo>/<slug>`, shared with native Claude Code and Orca. Part 2 is the manual
mechanics for non-harness repos.

## Part 1 — Harness verbs

### The merge rule (from `internal/worktree/merge.go`)

Promotion is **fast-forward only**: `Preflight` pins both OIDs and simulates with `git merge-tree`,
`IntegrateTarget` merges the pinned target **inside the worktree** and runs the verify gate there, `Promote`
runs `git merge --ff-only` on the main checkout and refuses any OID other than the one just verified. Never a
`--no-ff` merge commit on the live branch, never a real conflicting merge there. A predicted conflict stops
before anything mutates unless `--resolve-agent claude|codex` hands it to an agent inside the worktree.

### create

```bash
WT=$(claude-code worktree create "$SLUG" ${BRANCH:+--branch "$BRANCH"})   # resumes an existing slug safely
[ -d .venv ] && ln -sf "$(pwd)/.venv" "$WT/.venv"                          # Python repos: no 500 MB copy
cp .env "$WT/.env" 2>/dev/null; cp .claude/settings.local.json "$WT/.claude/settings.local.json" 2>/dev/null
```

A refusal (bad name, traversal, a foreign directory on `$SLUG`) prints the reason; never retry with a force
flag. Print path, branch, base SHA; launch with `cd $WT && claude`. Inside `~/.claude` the worktree is a
separate checkout exempt from hard rule 4, but `make install` and `claude-code` verbs still target the live
tree unless `CLAUDE_CONFIG_DIR` points at it.

### list

`claude-code worktree list` unions git's registration with the directories on disk and inspects each live
(dirty / merged / stale / orphaned / prunable / locked). Present its `reopen:` and `cleanup:` lines verbatim.

### merge

1. **Preview, always:** `claude-code worktree merge --dry-run "$SLUG"`. `clean — no predicted conflicts`
   exits 0; any `issue:` (dirty checkout, detached target, `--target` mismatch, unrelated histories,
   merge-tree failure, predicted `conflict [<files>]`) exits 1 with nothing mutated — report it and stop.
2. Ask: "Merge $SLUG into <target>? (Y/n)".
3. **Apply:** `claude-code worktree merge "$SLUG"` (`--verify "<cmd>"` overrides auto-detection from
   Makefile test / go.mod / pyproject / package.json / Cargo.toml; `--no-verify` is the only unverified path;
   no gate found refuses before integrating). A Makefile test target plus `go.mod` runs the scoped gate:
   `go build ./... && go vet ./...`, then `go test` on the packages `<target>...HEAD` changed (a non-Go file maps
   to its package, embedders and tests naming it), their importers (tests included, transitively) and the
   registry packages (`internal/{hooks,settings,cli,schemas}`, where present); a `go.mod`/`go.sum`
   change or an unowned non-markdown file tests `./...`. `--full-verify` runs `make test` instead;
   the full `make audit` stays the pre-push
   gate. Preflight re-runs; a failing gate stops with the worktree untouched on disk; success promotes, ticks
   the active plan for the promoted commits (`claude-code plan tick --range <old>..<new>`) and, unless
   `--no-clean`, cleans exactly as `clean` would. A failure
   after promotion prints a retry hint instead of leaving a merge commit; re-run step 1.
4. **Report:** `Merge complete` / `Branch: <branch> -> <target>` / `Verify: <cmd> pass|fail`.

**Predicted conflict:** `claude-code worktree merge --resolve-agent codex "$SLUG"` materializes it inside the
worktree, runs the agent once on the conflicted paths only (no `git add`/`commit` by the agent), then the
controller checks every marker is gone and nothing else changed, commits, revalidates ancestry, and continues
to promote. Markers left, an unexpected path, a silent codex→Claude fallback, a timeout or Ctrl-C fail the
operation with everything left on disk. `claude-code worktree merge --resume "$SLUG"` reattaches after a
crash without re-invoking the agent.

### clean / clean-all

`claude-code worktree clean --dry-run "$SLUG"`, confirm with the user, then `clean "$SLUG"`: it rechecks clean,
merged, unlocked and owned state live; the branch is deleted only when a plain `branch -d` succeeds.
`clean --force "$SLUG"` discards uncommitted and unmerged work instead of refusing — pass it only with the
user's explicit go, after showing what it is about to throw away. `clean-all --dry-run` lists `eligible:` /
`refused: … (reason)`; confirm "Remove N worktrees?", then `clean-all --yes`. Unregistered directories and
git's `prunable` entries are reported, never auto-removed.

## Part 2 — Manual control

Plain `git worktree` mechanics for repos without the harness verbs.

## Manual creation

### 1. Pick the directory (priority order)

```bash
ls -d ~/worktrees/"$(basename "$PWD")" 2>/dev/null   # harness default (outside the repo)
ls -d .worktrees 2>/dev/null          # common hidden alternative
grep -i "worktree.*director" CLAUDE.md 2>/dev/null   # explicit project preference
```

Use the first that exists. If none and no CLAUDE.md preference, ask the user before
creating one. Never scatter worktrees inside the main tree root where they'd be tracked.

### 2. Verify the directory is gitignored (project-local only)

```bash
git check-ignore -q .worktrees 2>/dev/null || echo "NOT IGNORED — fix first"
```

If not ignored, add it to `.gitignore` and commit **before** creating the worktree —
otherwise worktree contents get staged and pollute `git status`. (Directories outside the
repo need no check.)

### 3. Create the worktree + branch

```bash
slug=add-auth-endpoint; repo=$(basename "$PWD"); parent=$PWD
git worktree add ~/worktrees/$repo/$slug -b wt/$slug HEAD
cd ~/worktrees/$repo/$slug
```

### 4. Wire up the environment — symlink, don't reinstall

Prefer sharing the parent's `.venv` over a fresh install (instant, no 500MB copy). Capture `$parent`
**before** the `cd` above — after it, `$(pwd)/.venv` is the worktree's own path and self-links:

```bash
ln -sf "$parent/.venv" .venv
cp "$parent/.env" .env 2>/dev/null || true   # per-worktree override allowed
```

Only run a real install
(`make create-venv`, `uv sync`) if the branch changes dependencies and you need isolation from the
shared `.venv`.

### 5. Verify a clean baseline

Run the project's fast gate so new failures are distinguishable from pre-existing ones:

```bash
# the touched area's scoped tests, e.g. go test ./internal/<pkg> or pytest <path>
```

If the baseline is red, report it and ask before proceeding — don't start work on top of an
already-broken tree.

## Port allocation for parallel dev servers

Two worktrees both binding `:8000` collide. Assign each a deterministic, non-overlapping
block by slot index `n` (stride ≥ number of services):

```text
slot n:  app = 8000 + 10n   db = 5432 + 10n   redis = 6379 + 10n
slot 0:  8000 / 5432 / 6379
slot 1:  8010 / 5442 / 6389
slot 2:  8020 / 5452 / 6399
```

Persist the assignment in the worktree so it survives restarts and other worktrees can skip
taken slots:

```bash
cat > .worktree-ports.json <<'JSON'
{ "app": 8010, "db": 5442, "redis": 6389 }
JSON
```

To pick a free slot, read every existing `.worktree-ports.json` and choose the lowest `n`
whose ports are all unassigned. Feed the values into the dev command
(`uvicorn --port $(jq .app .worktree-ports.json)`) or a compose override
(`docker-compose.worktree.yml` mapping the allocated ports). Keep these overrides
**local-dev-only** — per-worktree ports must never reach a production deploy config.

## Cleanup with safety checks

Removing a worktree with unmerged commits or uncommitted changes loses work. Gate every
removal:

```bash
# 1. Anything uncommitted?
git -C ~/worktrees/$repo/$slug status --porcelain   # must be empty

# 2. Is the branch merged into the integration branch?
git branch --merged main | grep -q "wt/$slug" && echo "merged — safe to remove"

# 3. Remove only if both checks pass
git worktree remove ~/worktrees/$repo/$slug          # refuses if dirty (good)
git branch -d wt/$slug                                # -d refuses if unmerged (good)
git worktree prune                                    # clear stale metadata
```

- Use `git worktree remove --force` / `git branch -D` **only** when the work is
  intentionally discarded — confirm with the user first.
- A worktree is **stale** when its branch is merged or untouched for ~14+ days. Sweep
  periodically: `git worktree list` + check each branch's merge status and last commit date.
- `git worktree prune` after any manual `rm -rf` of a worktree path, or `git worktree list`
  shows phantom entries.

## Common mistakes

| Mistake | Fix |
|---------|-----|
| Worktree dir not gitignored | `git check-ignore` first; add + commit before creating |
| Reusing one port across branches | Allocate a port slot per worktree, persist to JSON |
| Sharing one DB/Redis across isolated branches | Isolate ports → isolate data stores |
| `rm -rf` a worktree, leaving phantom metadata | `git worktree remove` then `git worktree prune` |
| Force-removing a dirty worktree | Check `status --porcelain` + `--merged` first |
| Fresh `uv sync` per worktree (slow) | Symlink the shared `.venv` unless deps differ |
| Starting work on a red baseline | Run the touched area's scoped tests first; report failures before proceeding |

## Validation checklist

Before claiming a worktree is ready:

- [ ] `git worktree list` shows the expected path + `wt/<slug>` branch
- [ ] The worktree directory is gitignored (project-local)
- [ ] `.venv` symlink resolves; `.env` copied if the source has one
- [ ] `.worktree-ports.json` present with unique ports (if running a dev server)
- [ ] Baseline gate green (or failures reported and acknowledged)

Before removal:

- [ ] No uncommitted changes (`status --porcelain` empty)
- [ ] Branch merged (or removal explicitly confirmed as discard)
- [ ] No running process/container still bound to the worktree path
