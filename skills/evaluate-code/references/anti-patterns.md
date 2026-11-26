# Anti-patterns — the four principles as review findings

Principles: `rules/standards/principles.md` (adapted from Andrej Karpathy); this gallery and detectors are original.
Severity: `../SKILL.md` → Lenses → Discipline. Verbs: `detectors.md`.

## #1 Clarify Assumptions

- **A1. Silent interpretation:** “export all user data” leaves users, format and destination unstated.
  `claude-code plan assumptions`: `vague-action`, `unscoped-subject`. Surface choices and confirm scope.
- **A2. “Should work” plan:** replace hope with a runnable check, e.g. authenticated request → 200, anonymous → 401.
  `claude-code plan assumptions`: `hopeful`; `claude-code plan goals`: `vague`.

## #2 Simplicity First

Flag speculative generality; fix through `skills/simplify/SKILL.md` (`rules/standards/quality.md` → Simplify).
`claude-code review complexity` signals:

- **A3. Single-product factory:** `premature-abstraction`, `class-density`.
- **A4. Impossible-state error handling:** `cyclomatic-complexity`.
- **A5. 200 lines needing 50:** `function-length`, `nesting-depth`.
- **A6. Unused config knob:** `import-count` (often an unnecessary config library).

## #3 Surgical Changes

`claude-code review diff` signals:

- **A7. Drive-by reformat:** high noise ratio; keep the fix alone, reformat separately.
- **A8. Quote-style swap:** `quote-style-swap`; remove it from this diff.
- **A9. Untouched-code comment churn:** `docstring-addition`, `comment-only`; use a separate commit.

## #4 Goal-Driven Execution

`claude-code plan goals` signals:

- **A10. Unchecked “done”:** `none`; name a check, e.g. kill upstream, observe three retries then 503.
- **A11. No final verification:** `--json` → `has_final_verification: false`; run the real surface end-to-end.
- **A12. Numberless improvement:** `vague`; measure, e.g. p95 1.8s → 0.3s on 10k rows, benchmark asserts <0.5s.

## How the lens fits the review

Run detectors, then judge severity. Clean output rules out mechanical violations, not correctness/architecture defects.
