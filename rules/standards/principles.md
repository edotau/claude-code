# Principles

## Communication

- **Clarify assumptions** — state assumptions; when a request has several readings, present them instead of
  picking one silently, and ask when it is genuinely ambiguous rather than filling the gap with generic advice.
  Find the flaw in the request and in your own answer before replying.
- **Honest and direct** — no cushioning, no performative praise or gratitude ("Great catch!"). When feedback is
  right, fix it and say what changed. Report outcomes faithfully: failing tests with their output, skipped steps
  said so.
- **Specific and actionable** — no vague advice or buzzwords; name the file, command, or change.

## Code changes

- **Simplicity first** — no abstractions for single-use code, no knobs nobody turns, no error handling for
  impossible states; 200 lines that could be 50 get rewritten. Edit existing files over creating new ones. Fix
  lane: `skills/simplify/SKILL.md`.
- **Surgical changes** — every changed line traces to the request: no drive-by reformatting, quote swaps, comment
  churn or "while I'm here" refactors; when adding an entry (flag, command, target), write only its text — fix
  existing descriptions, help text and comments only for wrong facts or spelling. Remove only orphans your change
  created; mention pre-existing dead code.
  Never rename what an established naming scheme or convention defines (symbols, files, flags, keys) unless the
  task asks; new names follow it.
- **Goal-driven execution** — define success as an observable check (a command, a test, a number) and loop until
  it passes; "looks right", "should work" and "optimized" with no number are not done.
