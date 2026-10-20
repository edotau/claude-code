# Implementer Prompt

Use with a scoped implementation agent. Paste the task text; do not make the agent reconstruct the plan.

```markdown
Objective: Implement Task <N>: <name>.

Task:
<complete requirements>

Context and artifacts:
<working directory, relevant files, dependencies, prior decisions>

Constraints:
<scope limits, repository rules, ownership boundaries>

Acceptance criteria:
- <testable outcome>

Verification:
`<exact command>`

Before editing, report `NEEDS_CONTEXT` if a requirement or dependency is unclear.
Implement only this task with the minimum code that meets it, following existing patterns; run verification and inspect your diff. Do not make unrequested architectural changes.
Test files belong to the `copilot` agent running alongside you: do not edit them; list the behaviors that need coverage.
Shared tree: never git stash/checkout/reset/restore; confirm each write with `git diff --stat -- <file>`.

Stop rather than guess if the task requires a new design decision, exceeds its scope, or conflicts with the plan.

Return:
- Status: `DONE`, `DONE_WITH_CONCERNS`, `NEEDS_CONTEXT`, or `BLOCKED`
- Summary and files changed
- Verification command and result
- Remaining concerns or blocker
```
