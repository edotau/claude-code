# Anti-pattern gallery

Worked before/after examples of the four characteristic failures, each tagged with the
detector that catches it. These are the patterns to flag in review.

---

## #1 Think Before Coding

### A1. Silent interpretation of "export the data"
`assumption_linter.py` → `vague-action`, `unscoped-subject`

> "I'll just add a function to export all user data."

`just` hides scope; `all user data` is an absolute claim; *which* users, *what* format, *to
where* are all unstated. **Fix:** surface the choices — "Export the authenticated user's
profile rows to CSV; PII columns excluded. Confirm scope?"

### A2. "Should work" plan step
`assumption_linter.py` → `hopeful` · `goal_verifier.py` → `vague`

> "3. Wire the new endpoint — should work with the existing auth middleware."

Hopeful, not verified. **Fix:** "3. Wire the endpoint. verify: `curl -H 'Authorization: …'
/api/x` returns 200; unauthenticated returns 401."

---

## #2 Simplicity First

### A3. Factory for a single product
`complexity_checker.py` → `premature-abstraction`, `class-density`

```python
# BEFORE — abstraction with one implementer
class ExporterFactory:
    def create(self, kind: str) -> Exporter:
        if kind == "csv":
            return CsvExporter()
        raise ValueError(kind)

# AFTER — there is only CSV; call it
def export_csv(rows: list[dict]) -> str: ...
```

### A4. Error handling for impossible states
`complexity_checker.py` → `cyclomatic-complexity`

```python
# BEFORE — enum has two members; the else is dead
if status == Status.OK:
    ...
elif status == Status.FAIL:
    ...
else:
    raise RuntimeError("unreachable")  # speculative

# AFTER — exhaustive match, no dead branch
match status:
    case Status.OK: ...
    case Status.FAIL: ...
```

### A5. 200 lines that should be 50
`complexity_checker.py` → `function-length`, `nesting-depth`

A function that validates, transforms, persists, and notifies in one body nesting 5 levels
deep. **Fix:** four named functions, early returns, one orchestrator.

### A6. Config knob nobody turns
`complexity_checker.py` → `import-count` (often drags in a config lib)

A `timeout` parameter threaded through six layers, always called with the default. **Fix:**
inline the constant until a second caller actually needs to vary it (YAGNI).

---

## #3 Surgical Changes

### A7. Drive-by reformat
`diff_surgeon.py` → high noise ratio

A one-line bug fix whose diff also reflows 80 unrelated lines because the formatter ran on
save over the whole file. **Fix:** commit the fix alone; reformat in a separate, clearly
labeled commit if it's wanted at all.

### A8. Quote-style swap
`diff_surgeon.py` → `quote-style-swap`

```diff
- name = 'widget'
+ name = "widget"
```

No behavior change; pure churn that hides the real edit. **Fix:** revert it from this diff.

### A9. Docstring/comment churn on untouched code
`diff_surgeon.py` → `docstring-addition`, `comment-only`

Adding docstrings to functions the task never touched. Worthwhile as its own change — noise
inside a feature diff. **Fix:** separate commit.

---

## #4 Goal-Driven Execution

### A10. "Done" with no check
`goal_verifier.py` → `none`

> "Implemented the retry logic." (no test, no command, no criterion)

**Fix:** add the observable check — "verify: kill the upstream mid-request; client retries 3×
then surfaces a 503; `test_retry_exhausted` covers it."

### A11. Plan with no final verification
`goal_verifier.py` → missing-final

Six well-specified steps, no end-to-end step. Each unit can pass while the integration is
broken. **Fix:** add "7. Run the full suite + smoke the happy path end-to-end."

### A12. Metric claim without a number
`goal_verifier.py` → `vague` · `assumption_linter.py` → `vague-action`

> "Optimized the query."

Optimized to what? **Fix:** "Cut p95 from 1.8s → 0.3s on the 10k-row fixture; `bench_query`
asserts < 0.5s."
