# Task-brief review — vet a request before implementation

A skeptical, read-only pass that refines a request into clear orders; never implements. Run by the main session,
`Plan`, or `claude-code codex ask`: `rules/workflows/master-workflow.md` Phase 1 (request), Phase 4 (plan, below).
Verdict **READY | NEEDS CLARIFICATION** (`../SKILL.md` → Verdicts).

Input: the request and CLAUDE.md, with `git status --short` and `git diff --stat` for scope.

## Evaluation checklist

1. **Scope clarity** — target files identified? What NOT to touch? One task or several bundled?
2. **Acceptance criteria** — how do we know it's done? Measurable bar?
3. **Constraints** — flag violations of CLAUDE.md's hard rules and behavioral guidelines.
4. **Security surface** — auth/tokens, user input, filesystem, external APIs? Say so in the brief.
5. **Testing** — what runnable surface proves it live? Which spanning and regression tests (logic, not coverage:
   `rules/standards/testing-guidelines.md`)? Existing fixtures?
6. **Missing context** — what does the implementer lack? Prior art to reuse?

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

A well-scoped request is READY at once (don't invent problems); a TRIVIAL one gets a one-line brief.

## Plan review

Check a written plan against `agents/Plan.md` → Plan rules and Phase rules, plus:

1. **Evidence holds** — rerun two or three Target-shape rows' commands; a row that no longer reproduces, or a
   `Snapshot:` far behind HEAD in the files the plan touches, is an issue.
2. **The decision is real** — the `DECISION:` line names a rejected alternative that was viable; a straw man is an
   issue.
3. **Phase 0 can catch a regression** — its baseline covers the commands the later phases change.
4. **Tasks tick** — every task's `owns:` names real paths the phase edits, so `docs-plan-tick` can close it; a
   `codex`/`copilot`/`gemini`/`opencode` task names a `claude` fallback.
5. **A cold start works** — spec covered without scope creep, placeholder-free, each code change carries its code.

Flag only what would break implementation; wording, nice-to-haves and extra sections are not issues. Output the
verdict, then issues as `[Phase X.Y]: issue — why it matters`.
