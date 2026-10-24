# Dispatch Prompts

Every reviewer inspects the artifact itself (code, tests, plan), never the report describing it, returns findings as
file:line plus a concrete fix, and edits nothing. Paste the context each role needs; never make it reconstruct the plan.

## Implementer

```markdown
Objective: implement Task <N>: <name>.
Task: <complete requirements>
Context: <working directory, files, dependencies, prior decisions>
Owns: <files>. Do NOT edit others — agents are in this package concurrently.
Acceptance: <testable outcomes>
Verify: `<scoped command>`

Report NEEDS_CONTEXT before editing if anything is unclear. Implement only this task, minimally, following existing
patterns; stop rather than guess at a new design decision or scope growth. Test files belong to `copilot`: list the
behaviors that need coverage. Shared tree: never git stash/checkout/reset/restore; confirm each write with
`git diff --stat -- <file>`.

Return: status DONE | DONE_WITH_CONCERNS | NEEDS_CONTEXT | BLOCKED; files changed; verification output; concerns.
```

## Spec Reviewer

After implementation, before simplify and quality review.

```markdown
Review the implementation against the specification.
Specification: <requirements and acceptance criteria>
Implementation report: <implementer report>
Scope: <base..head or changed files>

Find: missing or misread requirements; unrequested behavior or scope; unsupported claims about completion or tests.
No style review.
Return SPEC_COMPLIANT with brief evidence, or SPEC_ISSUES with file:line findings.
```

## Code Quality Reviewer

Only after `SPEC_COMPLIANT` and the simplify pass.

```markdown
Load evaluate-code (Skill tool, or read ~/.claude/skills/evaluate-code/SKILL.md) and apply its lenses — the
performance lens on any hot path touched.
Task: <summary>
Scope: base <rev>, head <rev>, files <list>
Simplify report (if one ran; flagged items left on purpose): <report>

Complexity is the simplify pass's lane — raise it only where a flagged item hides a bug. Only issues this change introduced.
Return: verdict SHIP | FIX REQUIRED | BLOCK first; findings CRITICAL / HIGH / MEDIUM / LOW with file:line and a fix.
```

## Plan Reviewer

After a plan is written, before executing it.

```markdown
Review plan <PLAN_PATH> against spec <SPEC_PATH> for readiness.
Check: completeness (no TODOs, placeholders, missing steps); spec coverage without scope creep; clear task boundaries
and actionable steps; could an engineer follow it without getting stuck?
Flag only what would cause real problems during implementation; wording and nice-to-haves are not issues.
Return: Status Approved | Issues Found; issues as "[Task X, Step Y]: issue — why it matters"; optional recommendations.
```
