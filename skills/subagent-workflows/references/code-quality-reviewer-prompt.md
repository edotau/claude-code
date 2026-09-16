# Code Quality Reviewer Prompt

Use only after the spec reviewer returns `SPEC_COMPLIANT`.

```markdown
Review the implementation for correctness, maintainability, tests, security, and repository conventions.

Task:
<task summary and requirements>

Scope:
Base: <base revision>
Head: <head revision>
Files: <changed files>

Inspect the diff and relevant surrounding code. Check behavior, edge cases, error handling, test quality, unnecessary complexity, interface clarity, and regressions. Focus on issues introduced by this change.

Return:
- Strengths
- Findings ordered as Critical, Important, Minor; include file:line and a concrete fix
- Verdict: `APPROVED` or `CHANGES_REQUIRED`

Do not edit files.
```
