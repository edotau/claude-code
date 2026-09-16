# The grant-depth trap

## Symptom

A subagent or sandboxed session scoped to a path below the repo root reports:

```
fatal: not a git repository (or any parent up to mount point /path/to/repo)
```

...and packages installed in the project's own `.venv` are missing, often surfacing as a wall
of `ModuleNotFoundError` at import across many test suites — which reads like a regression but
is not.

## Cause

The scope boundary sits **below** the repo root. `.git` and the project `.venv` live *at* the
root; a grant on `<repo>/src` mounts only that subtree, so both sit above the boundary and are
simply invisible — no `git status`/`diff`/`log`/`commit`, and no importable venv packages.

## Fix

**Scope to the repository root, not a subdirectory.** This is why `/workflow:worktree`
symlinks the shared `.venv` at the worktree root rather than copying it into a subdir, and why
a code-workers leaf's file scope should still resolve `git`/imports from a root-level checkout
even when its *edit* scope is narrow.

## Diagnosing it quickly

```bash
cd <granted-path>
git rev-parse --show-toplevel   # fatal => .git is above the boundary
ls -d <repo-root>/.venv         # missing => venv is above the boundary too
```

If a mass of import errors appears in suites you have not touched, confirm they are
environmental rather than yours: revert your edit and re-run. Identical error counts mean the
failures happen at import, before any of your code executes.
