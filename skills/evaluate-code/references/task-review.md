# Task-brief review — vet a request before implementation

A skeptical staff engineer pass on a task request **before implementation begins**. The job is
NOT to implement — it is to **refine the request** so the implementing agent has crystal-clear
marching orders. Read-only intent: refine and judge, don't implement.

Any reviewer runs this — the main session, a Plan subagent, or a second agent via
`claude-code codex ask "<this file's checklist + the request>"`. It is the plan-vetting step of
`rules/workflows/master-workflow.md` Phase 1.

Verdict: **READY | NEEDS CLARIFICATION**. Verdict discipline: `../SKILL.md` → Verdicts.

## Input

The user's raw task request + the project's CLAUDE.md constraints; capture context with
`git status --short` and `git diff --stat` before judging scope.

## Evaluation checklist

1. **Scope clarity** — target files/modules identified? Boundary clear (what NOT to
   touch)? One task or several bundled?
2. **Acceptance criteria** — how do we know it's done? Measurable quality bar?
3. **Constraint awareness** — read CLAUDE.md's hard rules + behavioral guidelines; flag any
   violations.
4. **Security surface** — auth/tokens, user input, filesystem, external APIs? If yes,
   flag for the Security lens (`../SKILL.md` → Security) post-implementation.
5. **Testing expectations** — what runnable surface proves it live? Which spanning test and regression tests
   (logic, not a coverage number: `rules/standards/testing-guidelines.md`)? Existing fixtures?
6. **Missing context** — what does the implementer need that isn't in the request? Prior
   art to reuse?

## Output format

```markdown
## Refined Task Brief

**Objective**: [1 sentence — clear, specific, actionable]

**Scope**:
- Files: [files/modules to modify]
- Boundary: [what NOT to touch]

**Constraints** (from CLAUDE.md): [relevant constraints]

**Acceptance Criteria**:
- [ ] [specific, testable]
- [ ] Live check on the real surface shows the behavior; the spanning/regression tests pass

**Testing Requirements**: [spanning test + regression tests]

**Security Notes**: [implications, or "None identified"]

**Verdict**: READY / NEEDS CLARIFICATION

**Questions** (if NEEDS CLARIFICATION):
1. [specific question]   (at most 3)
```

## Guidelines

- Be concise. No filler. If well-scoped, mark READY immediately — don't invent problems.
- For TRIVIAL tasks (typos, one-liners), return a one-line brief and mark READY.
- Always check CLAUDE.md constraints — the most common source of implementation mistakes.

## Plan review

After a plan is written, before executing it.

Check it against `agents/Plan.md` → Plan rules and Phase rules (the one home; don't restate them here). The checks
only a reviewer can make:

1. **Evidence holds** — rerun two or three Target-shape rows' commands; a row that no longer reproduces, or a
   `Snapshot:` far behind HEAD in the files the plan touches, is an issue.
2. **The decision is real** — the `DECISION:` line names a rejected alternative that was viable; a straw man is an
   issue.
3. **Phase 0 can catch a regression** — its baseline covers the commands the later phases change.
4. **Tasks tick** — every task's `owns:` names real paths the phase edits, so `docs-plan-tick` can close it; a
   `coworker` task names a `code-worker` fallback.
5. **A cold start works** — could an engineer follow it without getting stuck: spec covered without scope creep,
   placeholder-free, each code change carries its code.

Flag only what would cause real problems during implementation; wording and nice-to-haves are not issues. A section the
plan added beyond the template is fine (the template is a floor).

Output: verdict READY | NEEDS CLARIFICATION; issues as `[Task X, Step Y]: issue — why it matters`.

Related, don't duplicate: `judge.md` → Protocol.
