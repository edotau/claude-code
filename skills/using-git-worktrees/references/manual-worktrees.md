# Manual Worktree Mechanics

The underlying steps for **manual control**. Inside a Claude Code session, prefer
`/workflow:worktree` (see the parent `SKILL.md`) — it bakes these in.

## Manual creation

### 1. Pick the directory (priority order)

```bash
ls -d .claude/worktrees 2>/dev/null   # harness default (gitignored)
ls -d .worktrees 2>/dev/null          # common hidden alternative
grep -i "worktree.*director" CLAUDE.md 2>/dev/null   # explicit project preference
```

Use the first that exists. If none and no CLAUDE.md preference, ask the user before
creating one. Never scatter worktrees inside the main tree root where they'd be tracked.

### 2. Verify the directory is gitignored (project-local only)

```bash
git check-ignore -q .claude/worktrees 2>/dev/null || echo "NOT IGNORED — fix first"
```

If not ignored, add it to `.gitignore` and commit **before** creating the worktree —
otherwise worktree contents get staged and pollute `git status`. (Directories outside the
repo need no check.)

### 3. Create the worktree + branch

```bash
slug=add-auth-endpoint
git worktree add .claude/worktrees/$slug -b wt/$slug HEAD
cd .claude/worktrees/$slug
```

### 4. Wire up the environment — symlink, don't reinstall

Prefer sharing the parent's `.venv` over a fresh install (instant, no 500MB copy):

```bash
ln -sf "$(git rev-parse --show-toplevel)/../.venv" .venv 2>/dev/null \
  || ln -sf "$(pwd)/.venv" .venv         # adjust to the real parent path
cp ../../.env .env 2>/dev/null || true   # per-worktree override allowed
```

Only run a real install
(`make create-venv`, `uv sync`) if the branch changes dependencies and you need isolation from the
shared `.venv`.

### 5. Verify a clean baseline

Run the project's fast gate so new failures are distinguishable from pre-existing ones:

```bash
make test-unit 2>/dev/null || make test 2>/dev/null || .venv/bin/python -m pytest -q
```

If the baseline is red, report it and ask before proceeding — don't start work on top of an
already-broken tree.

## Port allocation for parallel dev servers

Two worktrees both binding `:8000` collide. Assign each a deterministic, non-overlapping
block by slot index `n` (stride ≥ number of services):

```
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
git -C .claude/worktrees/$slug status --porcelain   # must be empty

# 2. Is the branch merged into the integration branch?
git branch --merged main | grep -q "wt/$slug" && echo "merged — safe to remove"

# 3. Remove only if both checks pass
git worktree remove .claude/worktrees/$slug          # refuses if dirty (good)
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
| Starting work on a red baseline | Run the fast gate first; report failures before proceeding |

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
