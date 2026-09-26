---
name: simplicity
description: 'Use after writing or modifying code, or when code feels too complex (harness variant of the built-in /simplify). Triggers "simplify this", "this is overcomplicated", "am I overcomplicating this", "reduce complexity", "reduce cognitive complexity", "too many abstractions", "clean this up", "this docstring is too long". For a full quality pass see the evaluate-code skill; for security see security-reviewer.'
---

# Simplicity

The **apply-the-fix** lens for two code-discipline principles — **Simplicity First (#2)** and
**Surgical Changes (#3)** — and the skill behind the simplify stage of `/workflow:orchestrate`.
Use it directly when simplifying in place, or hand it to a subagent over a diff.

**The lane:** behavior is preserved — you change *how* code is structured, never *what* it does.
Bug-hunting is `evaluate-code`'s job; security is `security-reviewer`'s.

## Simplicity First (#2)

CLAUDE.md → Behavioral guidelines #2 states the rule. **The test at simplify time:** would a
senior engineer call this overcomplicated? If yes, simplify. If you wrote 200 lines and it could
be 50, rewrite it. Worked before/after examples: `references/simplification-patterns.md` and the
`evaluate-code` skill's `references/anti-patterns.md`.

## Surgical Changes (#3)

CLAUDE.md → Behavioral guidelines #3 states the rule. Simplifying inverts the usual risk — the
failure mode here is **drive-by cleanup**, not over-engineering. Don't reformat/re-quote/
re-comment untouched code, don't "while I'm here" a refactor, and remove only what *your*
simplification made dead.

The one exception is **extended-scope consolidation**: if the diff duplicates a helper or
constant that already exists, consolidating at the source removes duplication the diff
created — that traces to the task. Say so explicitly (name the out-of-diff file + blast
radius). When the blast radius is large or unverifiable, flag it instead of reaching for it.

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

The ≤120-char / ≤2-line cap is canonical in `rules/standards/communication.md`; a bloated
docstring is over-engineering in prose, so it's in this lane. Say what the caller can't read
from the signature. Before/after pairs + the public-API exception: `references/docstring-style.md`.

## Automated first pass

Run the detectors first. `claude-code review complexity` is the #2 Simplicity detector;
`claude-code review diff` is the #3 Surgical detector (canonical in `evaluate-code`).

```bash
# Multi-commit changes: set BASE_SHA and use "$BASE_SHA..HEAD" in place of HEAD~1 below.
CHANGED=$(git diff --name-only HEAD~1 | grep -E '\.(py|ts|tsx|js|jsx)$')
# #2 Simplicity — per-function complexity, length, nesting, premature abstractions
[ -n "$CHANGED" ] && claude-code review complexity $CHANGED --threshold medium
# #3 Surgical — diff noise that doesn't trace to the task
claude-code review diff --diff HEAD~1..HEAD
```

`claude-code review complexity` estimates via indentation/keywords for Python and uses heuristics
for TS/JS (Go is not analyzed). Checks per-function cyclomatic complexity, function length, nesting depth; per-file
import count, class density, premature ABC/Protocol, file length. Thresholds `strict` (new
code) · `medium` (default) · `relaxed` (legacy); verdict `PASS`/`WARN`/`FAIL`, 0–100 score
per file. All detectors are advisory (exit 0) — use findings as leads, verify each by
reading the code; the detectors locate, you judge.

## Process

1. Run the automated first pass to find the hot spots.
2. Read each changed file (the detectors point; you decide).
3. Identify and rank by impact:
   - Single-use helpers/classes to inline (#2)
   - Duplicated logic to consolidate (#2)
   - Conditional chains and deep nesting to collapse with early returns (#2) — for
     method-level extraction, see `references/extract-method.md`
   - Speculative flexibility / impossible-state error handling to delete (#2)
   - Over-long docstrings to tighten (≤120 / 2 lines)
   - Diff noise that doesn't trace to the task (#3)
4. Apply the simplest fixes first, highest impact first. Preserve behavior; respect existing
   patterns. The success criterion is concrete, not "looks cleaner": **the same tests and
   lint that passed before the simplification pass after it** (`make test && make lint`,
   or the change's targeted tests). Run them — a green run is the proof behavior held, not a
   visual read of the diff. If there's no test covering the code you're about to restructure,
   that absence *is* the finding: say so before cutting, since you have no safety net.
5. Report what changed in this format:

```
[File] path/to/file.py
[Lines] 42-67
[Type] inline | consolidate | simplify | remove | docstring
[Scope] diff | extended   (extended = fix landed outside the diff; name file + blast radius)
[Impact] high | medium | low
[Description] What changed and why it's simpler — behavior unchanged.
```

If a fix would change behavior or its blast radius is unverifiable, **flag it, don't apply
it** — leave that call to the human.

## References

Local to this skill:
- `references/docstring-style.md` — the ≤120-char / 2-line rule, rationale, before/after.
- `references/simplification-patterns.md` — recurring wins: inline, consolidate, de-nest.
- `references/extract-method.md` — reduce a method's cognitive complexity by extracting focused helpers.

Shared with `evaluate-code` (canonical owner — read from `skills/evaluate-code/references/`):
- `anti-patterns.md` — over-abstraction gallery across Python, TS, shell.
- `karpathy-principles.md` — source attribution + when to relax each principle.
- `detectors.md` — full CLI reference for all detectors (incl. `claude-code review complexity`).
