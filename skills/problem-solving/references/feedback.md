# Review feedback

## Receive feedback

Iron rules:

1. **Verify before implementing** — reproduce the issue before changing code.
2. **YAGNI** — does the suggestion solve a real problem or a hypothetical one?
3. **Push back when justified** — if a change makes the code worse, say so with evidence
   (file, line, pattern) and propose an alternative.
4. **Order**: blocking issues (CRITICAL/HIGH/MEDIUM/LOW, all fixed in the same round without asking; only
   owner-judgment items go to a report) → simple fixes (typos, naming) → complex changes last.

## Replying

No performative agreement — `rules/standards/principles.md` is canonical: state the fix, not
thanks ("Fixed. Extracted PROGRESS_INTERVAL constant."). Wrong after pushing back: `"You were
right — I checked X and it does Y. Fixing now."` — factual, no apology.

Reply in the GitHub comment **thread**
(`claude-code scm comment <pr> --body-file F --reply-to <id>`), not as a top-level PR comment.
