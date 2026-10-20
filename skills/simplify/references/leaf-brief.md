# Simplify — Leaf Brief

Use after the spec reviewer returns `SPEC_COMPLIANT` and before the quality reviewer. The manager runs
`Skill(simplify)` in place when it holds the context; otherwise dispatch this brief to a `code-worker` leaf.
A leaf has no `Skill` tool, so the brief makes it read the skill as a file.

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
- Status: `SIMPLIFIED`, `NO_CHANGE`, or `BLOCKED` (gate red before you started)
```
