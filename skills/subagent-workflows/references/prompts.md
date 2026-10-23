# Dispatch Prompts

Shared preamble for every role below: inspect the artifact (code, tests, plan) directly — never
trust the report or summary describing it. Return findings as file:line plus a concrete fix. Do
not edit files unless you are the implementer. A fresh agent must be able to act from the brief
alone; restate what the role needs, do not make it reconstruct context.

## Implementer

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

## Spec Reviewer

Use after implementation and before quality review.

```markdown
Review the implementation against the task specification.

Specification:
<complete task requirements and acceptance criteria>

Implementation report:
<implementer report>

Scope:
<base/head revisions or changed files>

Identify:
- missing or incorrectly interpreted requirements;
- unrequested behavior or scope;
- unsupported claims about completion or tests.

Return one verdict:
- `SPEC_COMPLIANT`, with brief evidence; or
- `SPEC_ISSUES`, with actionable findings and file:line references.

Do not perform a general style review.
```

## Code Quality Reviewer

Use only after the spec reviewer returns `SPEC_COMPLIANT` and the simplify pass
([simplify/references/leaf-brief.md](../../simplify/references/leaf-brief.md)) has run.

```markdown
Review the implementation for correctness, maintainability, tests, security, and repository conventions.

Task:
<task summary and requirements>

Scope:
Base: <base revision>
Head: <head revision>
Files: <changed files>

Simplify report (already applied; flagged items were left on purpose):
<simplify report>

Check behavior, edge cases, error handling, test quality, interface clarity, and regressions. Complexity is
the simplify pass's lane: raise it only where a flagged item hides a bug. Focus on issues introduced by this change.

Return:
- Strengths
- Findings ordered as Critical, Important, Minor; include file:line and a concrete fix
- Verdict: `APPROVED` or `CHANGES_REQUIRED`
```

## Plan Reviewer

**Purpose:** Verify the plan is complete, matches the spec, and has proper task decomposition.
**Dispatch after:** The complete plan is written.

```text
Agent tool (general-purpose):
  description: "Review plan document"
  prompt: |
    You are a plan document reviewer. Verify this plan is complete and ready for implementation.

    **Plan to review:** [PLAN_FILE_PATH]
    **Spec for reference:** [SPEC_FILE_PATH]

    ## What to Check

    | Category | What to Look For |
    |----------|------------------|
    | Completeness | TODOs, placeholders, incomplete tasks, missing steps |
    | Spec Alignment | Plan covers spec requirements, no major scope creep |
    | Task Decomposition | Tasks have clear boundaries, steps are actionable |
    | Buildability | Could an engineer follow this plan without getting stuck? |

    ## Calibration

    **Only flag issues that would cause real problems during implementation.**
    An implementer building the wrong thing or getting stuck is an issue.
    Minor wording, stylistic preferences, and "nice to have" suggestions are not.

    Approve unless there are serious gaps — missing requirements from the spec,
    contradictory steps, placeholder content, or tasks so vague they can't be acted on.

    ## Output Format

    ## Plan Review

    **Status:** Approved | Issues Found

    **Issues (if any):**
    - [Task X, Step Y]: [specific issue] - [why it matters for implementation]

    **Recommendations (advisory, do not block approval):**
    - [suggestions for improvement]
```

**Reviewer returns:** Status, Issues (if any), Recommendations.
