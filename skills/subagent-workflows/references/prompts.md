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

## Simplifier

```markdown
Simplify the change below, behavior-preserving, by the procedure in ~/.claude/skills/simplify/SKILL.md.
Read that file first and follow it; its references are under ~/.claude/skills/simplify/references/.

You are a terminal leaf: run the four angles (reuse, simplification, efficiency, altitude) yourself in
one pass — do not dispatch agents — and say so in the summary.

Scope:
Base: <base revision>
Head: <head revision>
Files you may edit: <changed files>; an out-of-diff consolidation only as the skill's [extended] case.

Behavior gate (run before AND after your edits; both must be green):
`<task verification command>`

Shared tree: never git stash/checkout/reset/restore; confirm each write with `git diff --stat -- <file>`.

Return:
- One report block per applied change, in the skill's [File]/[Lines]/[Type]/[Scope]/[Impact]/[Description] format
- Flagged-not-cut items, each with the assumption you could not finish
- Behavior gate output, before and after
- Status: DONE (applied N, flagged M — N may be 0) | BLOCKED (gate red before you started)
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
Review plan <PLAN_PATH> against spec <SPEC_PATH> by
`~/.claude/skills/evaluate-code/references/task-review.md` → Plan review. Return its verdict first.
```
