# Anti-patterns — the four principles as review findings

The principles are in `rules/standards/principles.md` (adapted from Andrej Karpathy's observations on LLM coding
pitfalls); the detectors and this gallery are original. Severity per principle: `../SKILL.md` → Lenses →
Discipline. Detector verbs: `detectors.md`.

## #1 Clarify Assumptions

An ambiguous requirement gets one interpretation, silently; the damage is invisible until it ships.

- **A1. Silent interpretation** — `review assumptions` → `vague-action`, `unscoped-subject`.
  "I'll just add a function to export all user data": `just` hides scope; which users, what format, to where are
  unstated. **Fix:** surface the choices — "Export the authenticated user's profile rows to CSV, PII excluded.
  Confirm scope?"
- **A2. "Should work" plan step** — `review assumptions` → `hopeful`; `review goals` → `vague`.
  "3. Wire the new endpoint — should work with the existing auth middleware." **Fix:** "3. Wire the endpoint.
  verify: `curl -H 'Authorization: …' /api/x` returns 200; unauthenticated returns 401."

## #2 Simplicity First

Speculative generality; each piece adds reading cost for flexibility that never pays off. Before/after fixes:
`../../simplify/references/simplification-patterns.md`.

- **A3. Factory for a single product** — `review complexity` → `premature-abstraction`, `class-density`
- **A4. Error handling for impossible states** — `review complexity` → `cyclomatic-complexity`
- **A5. 200 lines that should be 50** — `review complexity` → `function-length`, `nesting-depth`
- **A6. Config knob nobody turns** — `review complexity` → `import-count` (often drags in a config lib)

## #3 Surgical Changes

Drive-by edits inflate the diff, bury the real change and make review and `git blame` harder.

- **A7. Drive-by reformat** — `review diff` → high noise ratio. A one-line fix whose diff also reflows 80 unrelated
  lines. **Fix:** commit the fix alone; reformat separately, if at all.
- **A8. Quote-style swap** — `review diff` → `quote-style-swap` (`'widget'` → `"widget"`). No behavior change.
  **Fix:** revert it from this diff.
- **A9. Docstring/comment churn on untouched code** — `review diff` → `docstring-addition`, `comment-only`.
  **Fix:** its own commit.

## #4 Goal-Driven Execution

"Looks right": work declared done with no concrete check.

- **A10. "Done" with no check** — `review goals` → `none`. "Implemented the retry logic." **Fix:** "verify: kill
  the upstream mid-request; client retries 3× then surfaces a 503; `test_retry_exhausted` covers it."
- **A11. Plan with no final verification** — `review goals` → missing-final. Each unit can pass while the
  integration is broken. **Fix:** add a final step that runs the change on its real surface end-to-end.
- **A12. Metric claim without a number** — `review goals` → `vague`. "Optimized the query." **Fix:** "p95 1.8s →
  0.3s on the 10k-row fixture; `bench_query` asserts < 0.5s."

## How the lens fits the review

Run the detectors first, then apply judgment: they point at hot spots and you decide severity. A clean run means
only the mechanical violations are absent; spend the attention on correctness, architecture and the Security lens.
