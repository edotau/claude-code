# Review and Simplify Task Template

Run claude in Review & Simplify mode over my current diff: apply `Skill(simplify)`, the comment
rule (`rules/standards/communication.md`), and the performance lens (`performance.md`)
on any hot path touched. Behavior preserved exactly; surgical changes only; match existing
project conventions; do not introduce new abstractions, dependencies, or patterns; do not expand
scope beyond the current diff; >80% confidence filter; consolidate similar issues into one finding.

Output:

- Must Fix / Should Fix / Nits / Questions
- Summary table + ship verdict
- For every simplification you applied, name the change and confirm
  behavior is preserved.
- Call out any comment blocks you trimmed with before/after line counts.

Ship verdict criteria:

- SHIP — no CRITICAL/HIGH, code is meaningfully simpler, comments
  are concise.
- FIX REQUIRED — small residual complexity or comment noise.
- BLOCK — structural complexity that requires design changes.

## Dispatch setup

```bash
BASE_SHA=$(git merge-base HEAD main)   # or: git diff --cached for staged only
git diff ${BASE_SHA}...HEAD --stat
make fmt && make lint                  # mechanical gate FIRST — can fail (must-fix)
```
