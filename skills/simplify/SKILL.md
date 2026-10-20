---
name: simplify
description: 'Behavior-preserving cleanup of changed code — overrides the built-in /simplify. Four parallel angles (reuse, simplification, efficiency, altitude), then applies fixes behind a behavior gate. Triggers "simplify this", "/simplify", "clean this up", "am I overcomplicating this", "too many abstractions", "reduce complexity". Bugs → evaluate-code; security → security-reviewer.'
---

# Simplify

The **apply-the-fix** lens for two code-discipline principles — **Simplicity First (#2)** and
**Surgical Changes (#3)** — and the skill behind the simplify stage of `/workflow:orchestrate`.
It overrides the built-in `/simplify`. Use it directly in place, or hand it to a subagent over a diff.

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

Commands and detector interpretation: [Automated first pass](references/review-process.md#automated-first-pass).

## Process

### Phase 1 — Review

Dispatch and the four review angles: [Phase 1 — Review](references/review-process.md#phase-1--review).

### Phase 2 — Apply

1. Run the automated first pass; dedup findings on the same line/mechanism.
2. For each, name the assumption the cut rests on (section above) — can't finish the sentence → flag, don't cut.
3. Skip fixes that change behavior or reach well outside the diff (note the skip). For method-level
   extraction see `references/extract-method.md`.
4. Run the same tests + lint before and after (`make test && make lint`, or the change's targeted tests).
   Green is the proof behavior held; no covering test is itself a finding — say so before cutting.
5. Report what changed in this format:

Output fields: [Report format](references/review-process.md#report-format).

End with a fixed/skipped summary. If a fix would change behavior or its blast radius is unverifiable,
**flag it, don't apply it** — leave that call to the human.

## References

Local to this skill:
- `references/docstring-style.md` — the ≤120-char / 2-line rule, rationale, before/after.
- `references/simplification-patterns.md` — recurring wins: inline, consolidate, de-nest.
- `references/extract-method.md` — reduce a method's cognitive complexity by extracting focused helpers.

Shared with `evaluate-code` (canonical owner — read from `skills/evaluate-code/references/`):
- `anti-patterns.md` — over-abstraction gallery across Python, TS, shell.
- `karpathy-principles.md` — source attribution + when to relax each principle.
- `detectors.md` — full CLI reference for all detectors (incl. `claude-code review complexity`).
