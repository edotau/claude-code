# Review process detail

## Automated first pass

```bash
# Multi-commit changes: set BASE_SHA and use "$BASE_SHA..HEAD" in place of HEAD~1 below.
CHANGED=$(git diff --name-only HEAD~1 | grep -E '\.(py|ts|tsx|js|jsx)$')
# #2 Simplicity — per-function complexity, length, nesting, premature abstractions
[ -n "$CHANGED" ] && claude-code review complexity $CHANGED --threshold medium
# #3 Surgical — diff noise that doesn't trace to the task
claude-code review diff --diff HEAD~1..HEAD
```

`claude-code review complexity` covers Python, TS/JS and Go. Checks per-function cyclomatic
complexity, function length, nesting depth; per-file import count, class density, premature
ABC/Protocol, file length. Thresholds `strict` (new
code) · `medium` (default) · `relaxed` (legacy); verdict `PASS`/`WARN`/`FAIL`, 0–100 score
per file. All detectors are advisory (exit 0) — use findings as leads, verify each by
reading the code; the detectors locate, you judge.

## Phase 1 — Review

Launch 4 agents in ONE message (Agent tool, `subagent_type: code-worker`, run in background). Each gets
the diff (`git diff @{upstream}...HEAD; git diff HEAD`, or the target argument) and one angle, and returns
findings as file, line, one-line summary, concrete cost. If the Agent tool is unavailable, do all four
angles yourself in one pass and say so in the summary.

- **Reuse** — flag new code that re-implements something the codebase already has; Grep shared/utility
  modules and files adjacent to the change, and name the existing helper to call instead.
- **Simplification** — flag unnecessary complexity the diff adds: redundant or derivable state, copy-paste
  with slight variation, deep nesting, dead code left behind, single-use abstractions, speculative
  flexibility, impossible-state error handling, over-long docstrings; name the simpler form.
- **Efficiency** — flag wasted work the diff introduces: redundant computation or repeated I/O, independent
  operations run sequentially, blocking work on startup or hot paths, long-lived closures capturing large
  scopes; name the cheaper form.
- **Altitude** — check each change fixes the root cause at the right depth rather than patching a symptom;
  special cases layered on shared infrastructure mean the fix isn't deep enough; name the more general change.

## Report format

```text
[File] path/to/file.py
[Lines] 42-67
[Type] inline | consolidate | simplify | remove | docstring
[Scope] diff | extended   (extended = fix landed outside the diff; name file + blast radius)
[Impact] high | medium | low
[Description] What changed and why it's simpler — behavior unchanged.
```
