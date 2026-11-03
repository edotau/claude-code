# Dispatch Prompts

Templates instantiating `../SKILL.md` → Handoff contract (same field names). Paste the context each role needs; never
make it reconstruct the plan. Leaf types and the `Agent:` label rule: [SKILL.md → Leaf types](../SKILL.md#leaf-types).

## Every brief adds

```text
Agent: <in-context | the agent this leaf requests via `claude-code ask --agent <x>`>
Shared tree: never git stash/checkout/reset/restore; confirm each write with `git diff --stat -- <file>`.
Terminal leaf: never dispatch subagents (an `Agent:` request is allowed) or commit.
Before a behavior change write TASK: code does X; check expects Y; spec says Z (Y vs Z disagree = stop, report).
After a defect fix write ISSUE: searched P, found N. Report both as plain sentences.
Return: status DONE | DONE_WITH_CONCERNS | NEEDS_CONTEXT | BLOCKED, then Ran on: <quoted `ask: ran on …` line, or
`self — <why>`>.
```

Reviewers additionally: inspect the artifact (code, tests, plan), never the report about it; return file:line findings
with a concrete fix; edit nothing; skip any dispatch or mechanical-gate steps the manager already ran.

## Implementer

```markdown
Objective: implement <name>: <complete requirements>.
Task: <plan task id; omit with no plan>
Context: <working directory, files, dependencies, prior decisions>
Owns: <files>. Do NOT edit others — agents are in this package concurrently.
Acceptance: <testable outcomes>
Verify: `<scoped command>`
Constraints: tests by a test agent per `rules/standards/testing-guidelines.md` (feature or bug fix only).
Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 4-6)

Report NEEDS_CONTEXT before editing if anything is unclear. Implement only this task, minimally, following existing
patterns; stop rather than guess at a new design decision or scope growth.
Return also: files changed; verification output; concerns.
```

## Researcher

```markdown
Objective: answer <question> for <caller requirement>.
Context: <facts already established, links supplied>. Do not re-derive them.
Sources: <primary sources to prefer>. Budget: <requests or minutes>; stop at it and return what you have.
Tools: <exact tool names and auth state>. On an auth failure or denial, stop that operation and report the tool and
the redacted error — no retry unchanged, no other tool or shell to get around it.
Links: fetch the ones supplied; after a 404, find the real link by search or site navigation, never guess a path.
Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 0-2)
Return also: each requirement → its evidence (URL or file:line, quoted where load-bearing) or the gap; unresolved items.
For a broad question prefer `claude-code dispatch explore` (parallel cited legs) over one researcher.
```

## Spec Reviewer

After implementation, before simplify and quality review.

```markdown
Specification: <requirements and acceptance criteria>
Implementation report: <implementer report>
Scope: <base..head or changed files>
Find: missing or misread requirements; unrequested behavior or scope; unsupported claims about completion or tests.
No style review. Return SPEC_COMPLIANT with brief evidence, or SPEC_ISSUES with file:line findings.
```

## Simplifier

```markdown
Simplify the change, behavior-preserving, by ~/.claude/skills/simplify/SKILL.md (read it first; its references are
under ~/.claude/skills/simplify/references/). Run the four angles (reuse, simplification, efficiency, altitude) yourself
in one pass and say so.
Scope: base <rev>, head <rev>. Files you may edit: <changed files>; an out-of-diff consolidation only as the skill's
[extended] case.
Behavior gate, before AND after (both green): `<task verification command>`
Return also: one [File]/[Lines]/[Type]/[Scope]/[Impact]/[Description] block per change; flagged-not-cut items with the
assumption you could not finish; gate output before and after; DONE (applied N, flagged M) | BLOCKED (gate red first).
```

## Code Quality Reviewer

Only after `SPEC_COMPLIANT` and the simplify pass.

```markdown
Apply ~/.claude/skills/evaluate-code/SKILL.md's Lenses and Verdicts (performance lens on any hot path touched).
Objective: review <summary>. Scope: base <rev>, head <rev>, files <list>.
Simplify report (flagged items left on purpose): <report>
Complexity is the simplify pass's lane; raise it only where a flagged item hides a bug. Only issues this change
introduced. Return: SHIP | FIX REQUIRED | BLOCK first; findings CRITICAL / HIGH / MEDIUM / LOW with file:line and a fix.
```

## Plan Reviewer

After a plan is written, before executing it.

```markdown
Review plan <PLAN_PATH> against spec <SPEC_PATH> by
`~/.claude/skills/evaluate-code/references/task-review.md` → Plan review. Return its verdict first.
```

## Lane leaf

One line of a lane batch ([lane-orchestrator.md](lane-orchestrator.md)); the attached lane plan carries the context.

```text
Task: <plan task id>
Objective: <checklist line, verbatim>. Edit only: <owned files>. Scratch: /tmp/claude-1000/<leaf>; delete it when done.
Done when: <scoped test> exits 0 and <live check> shows <expected>.
Return also: files changed, each command with exit code and env (cwd, CLAUDE_CONFIG_DIR), anything outside your files.
```
