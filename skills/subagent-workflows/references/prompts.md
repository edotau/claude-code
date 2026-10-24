# Dispatch Prompts

Every reviewer inspects the artifact itself (code, tests, plan), never the report describing it, returns findings as
file:line plus a concrete fix, and edits nothing. Paste the context each role needs; never make it reconstruct the plan.
Each template instantiates `../SKILL.md` → Handoff contract; its field names match the contract.

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

Before a behavior change write TASK: code does X; check expects Y; spec says Z (Y vs Z disagree = stop, report).
After a defect fix write ISSUE: searched P, found N.

Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 4-6)
Return: status DONE | DONE_WITH_CONCERNS | NEEDS_CONTEXT | BLOCKED; Ran on: <runner (model) · fallback none|who>; files
changed; verification output; the TASK/ISSUE facts as plain sentences; concerns.
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
- Ran on: <runner (model) · fallback none|who>
- Status: DONE (applied N, flagged M — N may be 0) | BLOCKED (gate red before you started)
```

## Code Quality Reviewer

Only after `SPEC_COMPLIANT` and the simplify pass.

```markdown
Load evaluate-code (Skill tool, or read ~/.claude/skills/evaluate-code/SKILL.md) and apply its lenses — the
performance lens on any hot path touched.
You are a read-only terminal reviewer: apply only its Lenses and Verdicts — skip its Dispatch a review and
mechanical-gate steps (the manager ran them); never edit files or dispatch agents.
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
You are a read-only terminal reviewer: apply only that procedure and skip any dispatch or mechanical-gate
steps (the manager ran them); never edit files or dispatch agents.
```

## Lane leaf

Brief for a lane orchestrator's leaf ([lane-orchestrator.md](lane-orchestrator.md)); the attached lane plan carries the
context.

```text
You are a terminal leaf: never dispatch, commit, or git stash/checkout/reset/restore.
Task: <checklist line, verbatim>. Edit only: <owned files>. Scratch: /tmp/claude-1000/<leaf>; delete it when done.
Done when: <scoped test> exits 0 and <live check> shows <expected>.
Before a behavior change write TASK: code does X; check expects Y; spec says Z (Y vs Z disagree = stop, report).
After a defect fix write ISSUE: searched P, found N.
Report: files changed, each command with exit code and env (cwd, CLAUDE_CONFIG_DIR), the TASK/ISSUE facts as
plain sentences (never raw tags), anything outside your files.
```
