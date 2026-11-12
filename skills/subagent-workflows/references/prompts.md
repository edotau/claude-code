# Dispatch Prompts

Templates for `../SKILL.md` → Phase 1 → Handoff contract (same fields); paste each role's context, never make it
rebuild the plan. `Agent: in-context` = the leaf does it itself.

## Every brief adds

```text
Agent: <in-context | work agent matching the selected leaf type>
Shared tree: never git stash/checkout/reset/restore; confirm each write with `git diff --stat -- <file>`. Terminal
leaf: never dispatch subagents or commit; external wrappers follow the shared relay contract.
Before a behavior change write TASK: code does X; check expects Y; spec says Z (Y vs Z disagree = stop, report).
After a defect fix write ISSUE: searched P, found N. Report both as plain sentences.
Return: status DONE | DONE_WITH_CONCERNS | NEEDS_CONTEXT | BLOCKED, Ran on: <quoted `ask: ran on …` line | `self — why`>.
```

Templates list only their own lines. Reviewers inspect the artifact, never the report about it, and edit nothing; Code
Quality Reviewer runs only after `SPEC_COMPLIANT` and the simplify pass.

## Implementer

```markdown
Objective: implement <name>: <complete requirements>.
Task: <plan task id; omit with no plan>
Context: <working directory, files, dependencies, prior decisions>
Owns: <files>. Do NOT edit others — agents are in this package concurrently.
Acceptance: <testable outcomes>
Verify: `<scoped command>`
Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 4-6)
NEEDS_CONTEXT before editing if unclear; this task only, minimally. Return also: files changed; verification; concerns.
```

## Researcher

```markdown
Objective: answer <question> for <caller requirement>.
Context: <facts already established, links supplied>; do not re-derive. Sources: <primary sources to prefer>.
Budget: <requests or minutes>; stop at it and return what you have.
Tools: <exact tool names and auth state>. On an auth failure or denial, stop and report the tool and the redacted error
— no unchanged retry, no other tool around it. Links: after a 404, search for the real link, never guess.
Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 0-2). Return also: each requirement → evidence or the gap.
```

## Spec Reviewer

```markdown
Specification: <requirements and acceptance criteria>. Implementation report: <implementer report>.
Scope: <base..head or changed files>. Find: missing or misread requirements; unrequested behavior or scope;
unsupported completion or test claims. No style review. Return SPEC_COMPLIANT with brief evidence, or SPEC_ISSUES
with file:line findings.
```

## Simplifier

```markdown
Simplify, behavior-preserving: `Skill(simplify)` under `rules/standards/quality.md` → Simplify (a terminal leaf runs
its four angles itself). Scope: base <rev>, head <rev>. May edit: <changed files>; out-of-diff only as the [extended]
case. Behavior gate, before AND after (both green): `<verification command>`
Return also: one [File]/[Lines]/[Type]/[Scope]/[Impact]/[Description] block per change; flagged-not-cut items with the
assumption left; gate output before/after; DONE (applied N, flagged M) | BLOCKED (gate red first).
```

## Code Quality Reviewer

```markdown
Apply ~/.claude/skills/evaluate-code/SKILL.md's Lenses and Verdicts (performance lens on any hot path).
Objective: review <summary>. Scope: base <rev>, head <rev>, files <list>. Simplify report: <report>
Only issues this change introduced (complexity is the simplify lane). Return SHIP | FIX REQUIRED | BLOCK first, then
CRITICAL / HIGH / MEDIUM / LOW findings with file:line and a fix.
```

## Plan Reviewer

```markdown
Review plan <PLAN_PATH> against spec <SPEC_PATH> by `~/.claude/skills/evaluate-code/references/task-review.md` →
Plan review. Return its verdict first.
```

## Lane leaf

```text
Task: <plan task id>
Objective: <checklist line, verbatim>. Edit only: <owned files>. Scratch: $TMPDIR/<leaf>; delete it when done.
Done when: <scoped test> exits 0 and <live check> shows <expected>. Return also: files changed, commands with exit
code and env (cwd, CLAUDE_CONFIG_DIR), anything outside your files.
```
