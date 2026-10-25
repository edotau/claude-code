---
name: simplify
description: 'Behavior-preserving cleanup of changed code — overrides the built-in /simplify. Four parallel angles (reuse, simplification, efficiency, altitude), then applies fixes behind a behavior gate. Triggers "simplify this", "/simplify", "clean this up", "am I overcomplicating this", "too many abstractions", "reduce complexity".'
---

# Simplify

The **apply-the-fix** lens for two code-discipline principles — **Simplicity First (#2)** and
**Surgical Changes (#3)**. It overrides the built-in `/simplify`. Use it directly in place, or hand it to a
subagent over a diff.

**The lane:** behavior is preserved — you change *how* code is structured, never *what* it does. Bugs and
security findings are reported, not fixed here.

## Simplicity First (#2) and Surgical Changes (#3)

CLAUDE.md → Behavioral guidelines 2 and 3 state the rules; this skill *applies* the fix, not just flags it.

Simplifying inverts the usual risk for #3: the failure mode here is **drive-by cleanup**, not
over-engineering — remove only what *your* simplification made dead.

The one exception is **extended-scope consolidation**: if the diff duplicates a helper or
constant that already exists, consolidating at the source removes duplication the diff
created — that traces to the task. Say so explicitly (name the out-of-diff file + blast
radius). When the blast radius is large or unverifiable, flag it instead of reaching for it.

Worked before/after examples: `references/simplification-patterns.md` (the one home for #2 fixes).

## Surface the assumption before you cut

Behavior preservation is the contract — but the trap is *assuming* you already know the
behavior. The fastest way to break code while "simplifying" is to delete an "impossible"
branch or collapse a conditional by guessing which path is dead. A wrong guess changes
behavior silently and the diff still looks like a cleanup.

Before applying a cut, name the assumption it rests on — out loud, from the code in front
of you:

- Deleting an `except` or guard → "this exception/state cannot occur because ___."
- Collapsing a branch → "this branch is unreachable because ___."
- Removing a parameter or knob → "no caller passes a non-default because ___ (grep'd)."

If you can't finish the sentence from the code, **flag it, don't cut it.** Verify
reachability with `grep`/tests before deleting anything you merely *suspect* is dead. This
is the simplify-lane form of "don't run with a wrong assumption" — the simplifier's one job
is to not change behavior, so it earns no license to guess.

## Docstring brevity

The 1-line / ≤120-char cap is canonical in `rules/standards/communication.md`; a bloated
docstring is over-engineering in prose, so it's in this lane. Say what the caller can't read
from the signature. Before/after pairs + the public-API exception: `references/docstring-style.md`.

Same lane for comments: remove section banners/ASCII dividers, TODOs that duplicate git
history, and restate-the-code comments; keep only why-comments; prefer a clear name over a comment.

## Automated first pass

```bash
# Multi-commit changes: set BASE_SHA and use "$BASE_SHA..HEAD" in place of HEAD~1 below.
CHANGED=$(git diff --name-only HEAD~1 | grep -E '\.(py|ts|tsx|js|jsx)$')
# #2 Simplicity — per-function complexity, length, nesting, premature abstractions
[ -n "$CHANGED" ] && claude-code review complexity $CHANGED --threshold medium
# #3 Surgical — diff noise that doesn't trace to the task
claude-code review diff --diff HEAD~1..HEAD
```

What each detector checks, languages covered, thresholds: `claude-code review complexity -h`,
`claude-code review diff -h`. Both are advisory (exit 0) — use findings as leads, verify each by reading
the code; the detectors locate, you judge.

## Process

### Phase 1 — Review

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
  scopes; name the cheaper form; a cost is a finding only with a measurement behind it — otherwise it is a
  hypothesis, say so.
- **Altitude** — check each change fixes the root cause at the right depth rather than patching a symptom;
  special cases layered on shared infrastructure mean the fix isn't deep enough; name the more general change.

### Phase 2 — Apply

1. Run the automated first pass; dedup findings on the same line/mechanism.
2. For each, name the assumption the cut rests on (section above) — can't finish the sentence → flag, don't cut.
3. Skip fixes that change behavior or reach well outside the diff (note the skip). For method-level
   extraction see `references/extract-method.md`.
4. Run the change's scoped tests + lint before and after, per `rules/standards/quality.md` →
   Verification Order (the full suite runs once at wrap-up, with the user's approval). Green is
   the proof behavior held; no covering test is itself a finding — say so before cutting.
5. Report what changed in this format:

```text
[File] path/to/file.py
[Lines] 42-67
[Type] inline | consolidate | simplify | remove | docstring
[Scope] diff | extended   (extended = fix landed outside the diff; name file + blast radius)
[Impact] high | medium | low
[Description] What changed and why it's simpler — behavior unchanged.
```

End with a fixed/skipped summary. If a fix would change behavior or its blast radius is unverifiable,
**flag it, don't apply it** — leave that call to the human.

## References

- `references/docstring-style.md` — the 1-line / ≤120-char rule, rationale, before/after.
- `references/simplification-patterns.md` — recurring wins: inline, consolidate, de-nest.
- `references/extract-method.md` — reduce a method's cognitive complexity by extracting focused helpers.
