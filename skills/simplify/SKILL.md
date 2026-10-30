---
name: simplify
description: 'Behavior-preserving cleanup of changed code — overrides the built-in /simplify. Four parallel angles (reuse, simplification, efficiency, altitude), then applies fixes behind a behavior gate. Triggers "simplify this", "/simplify", "clean this up", "am I overcomplicating this", "too many abstractions", "reduce complexity".'
---

# Simplify

Applies the fixes for **#2 Simplicity First** and **#3 Surgical Changes**
(`skills/evaluate-code/references/principles.md`); overrides the built-in `/simplify`. Run it in place or hand
it to a subagent over a diff.

**The lane:** change *how* code is structured, never *what* it does. Bugs and security findings are reported,
not fixed here. The failure mode is drive-by cleanup: remove only what *your* simplification made dead.

**Extended scope** is the one exception: when the diff duplicates a helper or constant that already exists,
consolidate at the source and say so (out-of-diff file + blast radius). A large or unverifiable blast radius
is flagged, not reached for.

## Name the assumption before you cut

The quickest way to break code while "simplifying" is to delete an "impossible" branch on a guess. Before each
cut, finish the sentence from the code in front of you:

- Deleting an `except` or guard → "this state cannot occur because ___."
- Collapsing a branch → "this branch is unreachable because ___."
- Removing a parameter or knob → "no caller passes a non-default because ___ (grep'd)."

Can't finish it → **flag it, don't cut it.** This rule governs every phase below.

Docstrings and comments are in the lane: the 1-line / ≤120-char cap (`rules/standards/communication.md`), no
banners or restate-the-code comments, a clear name over a comment. Before/after: `references/docstring-style.md`.

## Process

### Scope

The diff is `git diff @{upstream}...HEAD; git diff HEAD`, or the target argument; outside a git repo, the most
recently modified files. A `focus on <area>` argument (`/simplify focus on memory efficiency`) goes into every
brief and ranks its matching findings first.

### Phase 1 — Review

Run the advisory detectors (exit 0; leads to verify, not verdicts), then launch the four angles:

```bash
BASE=$(git merge-base @{upstream} HEAD 2>/dev/null || git rev-parse HEAD~1)
CHANGED=$(git diff --name-only "$BASE")
[ -n "$CHANGED" ] && claude-code review complexity $CHANGED   # length, nesting, branches (py/ts/js/go)
claude-code review diff --file <(git diff "$BASE")            # lines that don't trace to the task
```

Launch 4 agents in ONE message (Agent tool, `subagent_type: code-worker`, in background), each with the diff,
the focus and one angle, returning file, line, one-line summary, concrete cost. Without the Agent tool, do all
four yourself in one pass and say so.

- **Reuse** — new code re-implementing an existing helper (Grep shared modules and files next to the change)
  or logic duplicated within the diff; name the one place it should live.
- **Simplification** — redundant or derivable state, near-copy-paste, deep nesting, leftover dead code,
  single-use abstractions, speculative flexibility, impossible-state error handling, inconsistent naming,
  stringly-typed values, leaky abstractions, departures from `CLAUDE.md` and `rules/`; name the simpler form.
  Patterns: `references/simplification-patterns.md`.
- **Efficiency** — redundant computation, allocations or I/O, N+1 queries and per-item loops that could batch,
  independent work run sequentially, blocking work on startup or hot paths, needless frontend re-renders,
  closures holding large scopes; name the cheaper form. No measurement → call it a hypothesis.
- **Altitude** — does each change fix the root cause at the right depth? Special cases layered on shared
  infrastructure mean it doesn't; name the more general change.

### Phase 2 — Apply

1. Merge the angle and detector findings; dedup by line and mechanism. Drop false positives and findings not
   worth the change — skip them, don't argue them.
2. Skip any fix that changes behavior or reaches well outside the diff, and note the skip. Method-level
   extraction: `references/extract-method.md`.
3. Run the scoped tests + lint before and after (`rules/standards/quality.md` → Verification Order). Green is
   the proof behavior held; no covering test is itself a finding — say so before cutting.
4. Report each change, then a fixed/skipped summary:

```text
[File] path/to/file.py
[Lines] 42-67
[Type] inline | consolidate | simplify | remove | docstring
[Scope] diff | extended   (extended = fix landed outside the diff; name file + blast radius)
[Impact] high | medium | low
[Description] What changed and why it's simpler — behavior unchanged.
```
