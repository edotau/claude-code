# Spec Reviewer Prompt

Use after implementation and before quality review.

```markdown
Review the implementation against the task specification.

Specification:
<complete task requirements and acceptance criteria>

Implementation report:
<implementer report>

Scope:
<base/head revisions or changed files>

Inspect the code and tests directly; do not rely on the report. Identify:
- missing or incorrectly interpreted requirements;
- unrequested behavior or scope;
- unsupported claims about completion or tests.

Return one verdict:
- `SPEC_COMPLIANT`, with brief evidence; or
- `SPEC_ISSUES`, with actionable findings and file:line references.

Do not perform a general style review or edit files.
```
