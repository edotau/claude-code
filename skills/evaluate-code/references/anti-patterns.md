# Anti-patterns — the four principles as review findings

The four coding principles (`rules/standards/principles.md`) are adapted from Andrej Karpathy's observations on
LLM coding pitfalls (X post, 2025); the detectors and this gallery are original. Severity per principle:
`../SKILL.md` → Lenses → Discipline. Detector per principle: `detectors.md#detector-index`.

---

## #1 Clarify Assumptions

The failure: an ambiguous requirement gets one interpretation, silently. The code looks
finished, but it answered a question nobody confirmed. The damage is invisible until it
ships against the wrong assumption.

### A1. Silent interpretation of "export the data"

`review assumptions` → `vague-action`, `unscoped-subject`

> "I'll just add a function to export all user data."

`just` hides scope; `all user data` is an absolute claim; *which* users, *what* format, *to
where* are all unstated. **Fix:** surface the choices — "Export the authenticated user's
profile rows to CSV; PII columns excluded. Confirm scope?"

### A2. "Should work" plan step

`review assumptions` → `hopeful` · `review goals` → `vague`

> "3. Wire the new endpoint — should work with the existing auth middleware."

Hopeful, not verified. **Fix:** "3. Wire the endpoint. verify: `curl -H 'Authorization: …'
/api/x` returns 200; unauthenticated returns 401."

---

## #2 Simplicity First

The failure: speculative generality. A factory for one product, a Protocol with one
implementer, error handling for states that can't occur, a config knob nobody turns. Each
adds surface area and reading cost for flexibility that never pays off.

- **A3. Factory for a single product** — `review complexity` → `premature-abstraction`, `class-density`
- **A4. Error handling for impossible states** — `review complexity` → `cyclomatic-complexity`
- **A5. 200 lines that should be 50** — `review complexity` → `function-length`, `nesting-depth`
- **A6. Config knob nobody turns** — `review complexity` → `import-count` (often drags in a config lib)

The before/after fixes: `../../simplify/references/simplification-patterns.md`.

---

## #3 Surgical Changes

The failure: drive-by edits. Reformatting an untouched function, swapping quote styles,
churning comments, "while I'm here" refactors. They inflate the diff, bury the real change,
and make review and `git blame` harder.

### A7. Drive-by reformat

`review diff` → high noise ratio

A one-line bug fix whose diff also reflows 80 unrelated lines because the formatter ran on
save over the whole file. **Fix:** commit the fix alone; reformat in a separate, clearly
labeled commit if it's wanted at all.

### A8. Quote-style swap

`review diff` → `quote-style-swap`

```diff
- name = 'widget'
+ name = "widget"
```

No behavior change; pure churn that hides the real edit. **Fix:** revert it from this diff.

### A9. Docstring/comment churn on untouched code

`review diff` → `docstring-addition`, `comment-only`

Adding docstrings to functions the task never touched. Worthwhile as its own change — noise
inside a feature diff. **Fix:** separate commit.

---

## #4 Goal-Driven Execution

The failure: "looks right." Work declared done with no concrete check — no test added, no
command run, no observable criterion. It passes review on vibes and fails in production.

### A10. "Done" with no check

`review goals` → `none`

> "Implemented the retry logic." (no test, no command, no criterion)

**Fix:** add the observable check — "verify: kill the upstream mid-request; client retries 3×
then surfaces a 503; `test_retry_exhausted` covers it."

### A11. Plan with no final verification

`review goals` → missing-final

Six well-specified steps, no end-to-end step. Each unit can pass while the integration is
broken. **Fix:** add "7. Run the full suite + smoke the happy path end-to-end."

### A12. Metric claim without a number

`review goals` → `vague` · `review assumptions` → `vague-action`

> "Optimized the query."

Optimized to what? **Fix:** "Cut p95 from 1.8s → 0.3s on the 10k-row fixture; `bench_query`
asserts < 0.5s."

---

## How the lens fits the review

Run the detectors **first** (mechanical pass), then apply judgment on top. The detectors
are advisory — they point at hot spots; you decide severity. A clean detector run does not
mean the change is good; it means the mechanical violations are absent and you can spend
attention on correctness, architecture, and the Security lens (`../SKILL.md` → Security).
