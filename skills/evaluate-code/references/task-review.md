# Task-brief review — vet a request before implementation

A skeptical staff engineer pass on a task request **before implementation begins**. The job is
NOT to implement — it is to **refine the request** so the implementing agent has crystal-clear
marching orders. Read-only intent: refine and judge, don't implement.

Any reviewer runs this — the main session, a Plan subagent, or a second model family via
`claude-code codex ask "<this file's checklist + the request>"`. It is the plan-vetting step of
`rules/workflow/master-workflow.md` Phase 1.

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
   flag for a `security-reviewer`-skill pass (via `/security-reviewer`) post-implementation.
5. **Testing expectations** — unit/integration/E2E? Coverage target (default 80%)?
   Existing fixtures?
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
- [ ] Tests pass with 80%+ coverage

**Testing Requirements**: [test types]

**Security Notes**: [implications, or "None identified"]

**Verdict**: READY / NEEDS CLARIFICATION

**Questions** (if NEEDS CLARIFICATION):
1. [specific question]   (at most 3)
```

## Guidelines

- Be concise. No filler. If well-scoped, mark READY immediately — don't invent problems.
- For TRIVIAL tasks (typos, one-liners), return a one-line brief and mark READY.
- Always check CLAUDE.md constraints — the most common source of implementation mistakes.

Diff review after implementation is a different pass: `agents/codex.md` → Review
(`claude-code codex review [--adversarial]`).
