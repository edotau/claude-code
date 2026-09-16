Run claude in Review & Simplify mode over my current diff.

Priorities in this exact order:

1. Simplicity — this is the top priority.
   - Apply the simplicity skill aggressively.
   - Collapse single-use abstractions.
   - Inline premature indirection.
   - Remove speculative flexibility and dead branches.
   - Flatten nesting where it reads better.
   - Delete code that exists "just in case."
   - Prefer fewer files, fewer layers, fewer concepts.

2. Comment hygiene.
   - Trim verbose comment blocks to concise, purposeful comments.
   - Remove restate-the-code comments.
   - Remove section banners and ASCII dividers.
   - Remove TODOs that duplicate the git history.
   - Keep only comments that explain *why*, not *what*.
   - Prefer a one-line comment over a paragraph.
   - Prefer a clear name over any comment at all.

3. Performance.
   - Run the performance-lens checks (`references/performance.md`) on any hot path touched.
   - Flag N+1 queries, redundant loops, unnecessary allocations.
   - Only optimize where profile evidence or clear complexity warrants it.
   - Never sacrifice simplicity for micro-optimizations.

Rules of engagement:

- Behavior must be preserved exactly.
- Surgical changes only — no drive-by reformatting.
- Match existing project conventions.
- Do not introduce new abstractions, dependencies, or patterns.
- Do not expand scope beyond the current diff.
- Confidence filter: only report issues you're >80% sure about.
- Consolidate similar issues into one finding.

Output:

- Must Fix / Should Fix / Nits / Questions
- Summary table + ship verdict
- For every simplification you applied, name the change and confirm
  behavior is preserved.
- Call out any comment blocks you trimmed with before/after line counts.

Ship verdict criteria:

- Ship it — no CRITICAL/HIGH, code is meaningfully simpler, comments
  are concise.
- Ship after must-fixes — small residual complexity or comment noise.
- Needs rework — structural complexity that requires design changes.
