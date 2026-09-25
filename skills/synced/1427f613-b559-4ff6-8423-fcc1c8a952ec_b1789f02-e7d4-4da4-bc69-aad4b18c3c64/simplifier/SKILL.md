---
name: simplifier
description: 'Use after writing or modifying code, or when code feels too complex. Triggers "simplify this", "this is overcomplicated", "am I overcomplicating this", "reduce complexity", "reduce cognitive complexity", "too many abstractions", "clean this up", "this docstring is too long". For a full quality pass see the code-review skill; for security see security-reviewer.'
---

# Simplifier

Reduce the complexity of code you just wrote or changed. This is the **apply-the-fix** lens
for two of the four code-discipline principles — **Simplicity First (#2)** and **Surgical
Changes (#3)** — the keyword-triggered skill behind the `code-engineer` agent's simplify phase.

Use the `code-engineer` **agent** (or `/code-engineer simplify`) when you want it driven as
part of the coding lifecycle. Use this **skill** directly when you're simplifying in place.
Either way: behavior is preserved — you change *how* code is structured, never *what* it does.
Bug-hunting is `code-review`'s
job; security is `security-reviewer`'s. Stay in the simplicity lane.

## Simplicity First (#2)

**Minimum code that solves the problem. Nothing speculative.**

- No features beyond what was asked.
- No abstractions for single-use code.
- No "flexibility" or "configurability" that wasn't requested.
- No error handling for impossible scenarios.
- If you write 200 lines and it could be 50, rewrite it.

**The test:** would a senior engineer call this overcomplicated? If yes, simplify. Worked
before/after examples: `references/simplification-patterns.md` and the `code-review` skill's
`references/anti-patterns.md`.

## Surgical Changes (#3)

**Every changed line traces to the task.** When simplifying, the risk is the opposite of
over-engineering: drive-by cleanup. Flag and resist —

- Don't reformat, re-quote, or re-comment untouched code.
- Don't refactor things that aren't broken to "while I'm here" them.
- Match the existing style even where you'd do it differently.
- Remove only the imports/vars/helpers *your* simplification made dead. Mention pre-existing
  dead code; don't delete it unasked.

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

Keep docstrings to **≤120 characters or ≤2 lines** — a one-line summary that says what the
caller can't already read from the signature. A bloated docstring is over-engineering in
prose. Full rule + before/after pairs and the public-API exception:
`references/docstring-style.md`.

## Automated first pass

Run the stdlib detectors first. `simplifier` is the **canonical owner** of the #2 Simplicity
detector `complexity_checker.py` (it lives in this skill's `scripts/`). The #3 Surgical
detector `diff_surgeon.py` stays canonical in `code-review`. Resolve each dir from the
project, falling back to global:

```bash
# #2 Simplicity — canonical here in simplifier/scripts/
SIMP="${CLAUDE_PROJECT_DIR:-$HOME}/.claude/skills/simplifier/scripts"
[ -d "$SIMP" ] || SIMP="$HOME/.claude/skills/simplifier/scripts"
# #3 Surgical — canonical in code-review/scripts/
CR="${CLAUDE_PROJECT_DIR:-$HOME}/.claude/skills/code-review/scripts"
[ -d "$CR" ] || CR="$HOME/.claude/skills/code-review/scripts"

CHANGED=$(git diff --name-only HEAD~1 | grep -E '\.(py|ts|tsx|js|jsx)$')
# #2 Simplicity — per-function complexity, length, nesting, premature abstractions
[ -n "$CHANGED" ] && python3 "$SIMP/complexity_checker.py" $CHANGED --threshold medium
# #3 Surgical — diff noise that doesn't trace to the task
python3 "$CR/diff_surgeon.py" --diff HEAD~1..HEAD
```

`complexity_checker.py` is stdlib-only (AST for Python; regex/indentation heuristics for
TS/JS). Checks per-function cyclomatic complexity, function length, nesting depth; per-file
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
   lint that passed before the simplification pass after it** (`make test-fast && make lint`,
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
- `scripts/complexity_checker.py` — the #2 Simplicity detector (canonical home; stdlib-only).
- `references/docstring-style.md` — the ≤120-char / 2-line rule, rationale, before/after.
- `references/simplification-patterns.md` — recurring wins: inline, consolidate, de-nest.
- `references/extract-method.md` — reduce a method's cognitive complexity by extracting focused helpers.

Shared with `code-review` (canonical owner — read from `skills/code-review/references/`):
- `anti-patterns.md` — over-abstraction gallery across Python, TS, shell.
- `karpathy-principles.md` — source attribution + when to relax each principle.
- `detectors.md` — full CLI reference for all detectors (incl. `complexity_checker.py`).
