# Worktree Mechanics: harness verbs and manual control

Part 1 is the procedure behind `claude-code worktree` and the `worktree-create`/`worktree-remove` hooks
(`internal/worktree`); every verb resolves the root and each worktree's real branch from git — never assume a
naming convention. Root: `~/worktrees/<repo>/<slug>`. Part 2 is for non-harness repos.

## Part 1 — Harness verbs

### The merge rule (from `internal/worktree/merge.go`)

Promotion is **fast-forward only**: `Preflight` pins both OIDs and simulates with `git merge-tree`,
`integrateTarget` merges the pinned target **inside the worktree** and runs the verify gate there, `promote`
runs `git merge --ff-only` on the main checkout and refuses any OID other than the one just verified. Never a
`--no-ff` merge commit on the live branch, never a real conflicting merge there. A predicted conflict stops
before anything mutates unless `--resolve-agent <agent>` hands it to an agent inside the worktree.

### create

```bash
WT=$(claude-code worktree create "$SLUG" ${BRANCH:+--branch "$BRANCH"})   # resumes an existing slug safely
cp .claude/settings.local.json "$WT/.claude/settings.local.json" 2>/dev/null
```

Seeding (`.venv` symlink, `.env` copy) and the harness-repo caveat: `SKILL.md`. An existing worktree is not reseeded.
A refusal (bad name, traversal, foreign directory on `$SLUG`) prints the reason; never retry with a force flag.
Print path, branch, base SHA; launch with `cd $WT && claude`.

### list

`claude-code worktree list` unions git's registration with the directories on disk and inspects each live
(dirty / merged / stale / orphaned / prunable / locked). Present its `reopen:` and `cleanup:` lines verbatim.

### merge

1. **Preview, always:** `claude-code worktree merge --dry-run "$SLUG"`. `clean — no predicted conflicts`
   exits 0; any `issue:` (dirty worktree, detached target, `--target` mismatch, unrelated histories,
   merge-tree failure, predicted `conflict [<files>]`) exits 1 with nothing mutated — report it and stop.
   A dirty target is refused only when its uncommitted paths overlap files the merge touches; the issue names
   who claims each one. A predicted conflict also queues a notice for the worktree's owning session.
2. Ask: "Merge $SLUG into <target>? (Y/n)".
3. **Apply:** `claude-code worktree merge "$SLUG"`. `--verify "<cmd>"` overrides auto-detection (Makefile test /
   go.mod / pyproject / package.json / Cargo.toml); `--no-verify` is the only unverified path; no gate found
   refuses before integrating. Makefile test target plus `go.mod` runs the scoped gate: `go build ./... &&
   go vet ./...`, then `go test` on the packages `<target>...HEAD` changed (a non-Go file maps to its package,
   embedders and tests naming it), their importers (tests included, transitively) and the registry packages
   (`internal/{hooks,settings,cli,schemas}`, where present); a `go.mod`/`go.sum` change or an unowned
   non-markdown file tests `./...`. `--full-verify` runs `make test`; the full `make audit` stays the
   pre-push gate. Preflight re-runs; a failing gate stops with the worktree untouched; success promotes,
   ticks the active plan (`claude-code plan tick --range <old>..<new>`) and, unless `--no-clean`, cleans as
   `clean` would. A failure after promotion prints a retry hint, not a merge commit; re-run step 1.
4. **Report:** `Merge complete` / `Branch: <branch> -> <target>` / `Verify: <cmd> pass|fail`.

**Predicted conflict:** `claude-code worktree merge --resolve-agent codex "$SLUG"` materializes it in the
worktree and runs the agent once on the conflicted paths only (no `git add`/`commit`); the controller then checks
every marker is gone and nothing else changed, commits, revalidates ancestry and promotes. Markers left, an
unexpected path, a silent codex→Claude fallback, a timeout or Ctrl-C fail the operation with everything left on
disk. `merge --resume "$SLUG"` reattaches after a crash without re-invoking the agent.

### clean / clean-all

`claude-code worktree clean --dry-run "$SLUG"`, confirm with the user, then `clean "$SLUG"`: it rechecks clean,
merged, unlocked and owned state live; the branch is deleted only when a plain `branch -d` succeeds.
`clean --force` discards uncommitted and unmerged work — only with the user's explicit go, after showing what
is thrown away. `clean-all --dry-run` lists `eligible:` / `refused: … (reason)`; confirm "Remove N worktrees?",
then `clean-all --yes`. Unregistered directories and `prunable` entries are reported, never auto-removed.

## Part 2 — Manual control

Plain `git worktree` mechanics for repos without the harness verbs.

## Manual creation

### 1. Pick the directory (priority order)

```bash
ls -d ~/worktrees/"$(basename "$PWD")" 2>/dev/null   # harness default (outside the repo)
ls -d .worktrees 2>/dev/null          # common hidden alternative
grep -i "worktree.*director" CLAUDE.md 2>/dev/null   # explicit project preference
```

Use the first that exists; if none and no CLAUDE.md preference, ask the user. Never put worktrees inside the main
tree root where they'd be tracked.

### 2. Verify the directory is gitignored (project-local only)

```bash
git check-ignore -q .worktrees 2>/dev/null || echo "NOT IGNORED — fix first"
```

If not ignored, add it to `.gitignore` and commit **before** creating the worktree, or its contents pollute
`git status`. Directories outside the repo need no check.

### 3. Create the worktree + branch

```bash
slug=add-auth-endpoint; repo=$(basename "$PWD"); parent=$PWD
git worktree add ~/worktrees/$repo/$slug -b wt/$slug HEAD
cd ~/worktrees/$repo/$slug
```

### 4. Wire up the environment — symlink, don't reinstall

Capture `$parent` **before** the `cd` above — after it, `$(pwd)/.venv` is the worktree's own path and self-links:

```bash
ln -sf "$parent/.venv" .venv
cp "$parent/.env" .env 2>/dev/null || true   # per-worktree override allowed
```

Run a real install (`make create-venv`, `uv sync`) only if the branch changes dependencies.

### 5. Verify a clean baseline

Run the touched area's scoped tests (e.g. `go test ./internal/<pkg>`, `pytest <path>`) so new failures are
distinguishable. A red baseline: report it and ask before proceeding.

## Port allocation for parallel dev servers

Two worktrees binding `:8000` collide. Give each a block by slot index `n` (stride ≥ number of services):

```text
slot n:  app = 8000 + 10n   db = 5432 + 10n   redis = 6379 + 10n
slot 1:  8010 / 5442 / 6389
```

Persist it in the worktree so it survives restarts and others can skip taken slots:

```bash
cat > .worktree-ports.json <<'JSON'
{ "app": 8010, "db": 5442, "redis": 6389 }
JSON
```

Pick the lowest `n` whose ports no existing `.worktree-ports.json` uses. Feed the values into the dev command
(`uvicorn --port $(jq .app .worktree-ports.json)`) or a compose override (`docker-compose.worktree.yml`). Keep
these **local-dev-only** — never in a production deploy config.

## Cleanup with safety checks

Removal with unmerged or uncommitted work loses it. Gate every removal:

```bash
git -C ~/worktrees/$repo/$slug status --porcelain   # must be empty
git branch --merged main | grep -q "wt/$slug" && echo "merged — safe to remove"
git worktree remove ~/worktrees/$repo/$slug          # refuses if dirty (good)
git branch -d wt/$slug                                # -d refuses if unmerged (good)
git worktree prune                                    # clear stale metadata
```

- `git worktree remove --force` / `git branch -D` **only** for intentionally discarded work, after user confirmation.
- **Stale** = branch merged or untouched ~14+ days; sweep with `git worktree list` + each branch's merge status and
  last commit date.
- `git worktree prune` after any manual `rm -rf` of a worktree path, or when `git worktree list` shows phantoms.

## Common mistakes

- Unignored worktree dir, one port or DB shared across branches, `rm -rf` instead of `git worktree remove` +
  `prune`, force-removing a dirty tree, fresh `uv sync` per worktree, or working on a red baseline: each is fixed by
  the step above that covers it.

## Validation checklist

Ready: `git worktree list` shows the path + `wt/<slug>`; dir gitignored; `.venv` symlink resolves; `.env` copied;
`.worktree-ports.json` unique (if serving); baseline green or failures acknowledged.
Removal: `status --porcelain` empty; branch merged (or discard confirmed); no process bound to the path.
