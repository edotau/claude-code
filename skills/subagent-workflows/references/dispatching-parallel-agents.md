# Dispatching Parallel Agents

Use parallel fan-out for two or more tasks with no dependency, shared state, or overlapping writes.

Do not use it when one fix may resolve the others, or when agents need the same evolving context.

## Overlapping files are not automatically disqualifying

"Tasks touch the same package" and "tasks touch the same file" are different problems. Same package
with **disjoint new files** parallelizes cleanly; same file does not.

When several tasks land in one package, split them so each agent CREATES its own new file and is
explicitly forbidden from editing existing ones. The manager then does the integration edit — the
shared file — alone. This turns a serial job into a parallel one without a single shared write.

```
LANE 1 -> internal/router/tokencache.go   (new, owned)
LANE 2 -> internal/router/authretry.go    (new, owned)
LANE 3 -> internal/router/diag.go         (new, owned)
MANAGER -> router.go                     (the integration, done once, by one writer)
```

Say "do NOT edit `<existing files>` — other agents are in this package concurrently" in every brief.
Ask each lane to report the exact API surface it produced, so integration needs no re-reading.

## Process

1. Partition by ownership, not by topic — one writer per file, always.
2. Give each agent one objective and its exclusive file list.
3. Build each handoff using the contract in [SKILL.md](../SKILL.md).
4. Issue all `Agent` calls in one message so they run concurrently.
5. Dispatch a test agent in the SAME message (`rules/standards/quality.md` requires it; a test agent
   queued behind the implementers is the serial pass that rule exists to prevent).
6. Review results, integrate, then run the combined verification yourself.

## This repo's shared tree — put these in every brief

A checkout shared by parallel subagents (and `~/.claude` itself, shared by every live `claude`
session) is ONE working tree. Every git command that writes the tree is global.

- **NEVER `git stash` / `checkout` / `reset` / `restore`** — a `git stash` "to check whether a failure
  was pre-existing" destroys every sibling lane's uncommitted work.
- **Gate on a SCOPED target** (`go build ./internal/<pkg>/...`, `go test ./internal/<pkg>/`). A
  repo-wide failure mid-fan-out is usually a sibling's half-finished edit, not yours. Diffing your
  own assigned files is how you attribute a failure — never stash to find out.
- **Verify survival**: run `git diff --stat -- <file>` right after writing each file, and again before
  reporting. A file that shows zero changes later was wiped; redo it.
- **Commit each verified subset early.** A commit is the only state a stray checkout cannot revert.
  Stage the lane's files explicitly; never `git add -A` while other lanes are mid-write.

Prefer new-file ownership over `isolation: "worktree"` where it fits. Worktree isolation is the least
reliable part of a fan-out — lanes have died with `.claude/worktrees/agent-*` vanishing mid-run.

## Example brief

```
Agent(
  subagent_type="code-workers",
  description="Token cache for the router",
  prompt="""
  Own ONLY: internal/router/tokencache.go and tokencache_test.go (both new).
  Do NOT edit router.go — other agents are in this package concurrently.

  Objective: in-memory, provider-keyed credential cache with single-flight and a refresher.
  Constraints: keyed by provider name, never process-global; the command-credential TTL in
    providers/creds.go stays authoritative; never log a credential.
  Test RED first: assert a cache HIT performs zero mints (count via an injected seam).
  Verify: gofmt -l <files> && go build ./... && go test -race ./internal/router/ -count=1
  Shared tree: no stash/checkout/reset/restore; confirm each write with git diff --stat.
  Return: files, exact API surface to wire, verbatim RED output, gate results, risks.
  """
)
```

## Fan-in checklist

- Treat every report as a **claim**, not a result. Two real failure modes from one session: a lane
  reported a gate failure that was actually a sibling's mid-flight package move, and a lane's new
  test passed even with the feature under test disabled.
- For a test guarding an invariant, confirm it can FAIL — disable the thing it protects and re-run.
  A test that has never failed is not yet a guard.
- Inspect the integrated diff for changes outside every lane's assigned scope.
- Re-run scoped checks, then the full suite once (`make test`). Add `-race` for anything concurrent.
- Rebuild before believing a binary's behavior — `go test` green says nothing about `bin/`.
- Synthesize only verified results.

For repeatable fan-out or multi-stage work, see [choosing-a-pattern.md](choosing-a-pattern.md).
