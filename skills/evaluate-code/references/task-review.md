# Task-brief review — vet a request before implementation

Read-only: refine the request, never implement. Run in main, `Plan`, or
`claude-code agents ask --overlay '{"agent":"codex"}'`; `rules/workflows/master-workflow.md` phases 1 and 4.
Input: request, CLAUDE.md, `rtk git status --short`, `rtk git diff --stat`.
Verdicts: `../SKILL.md` → Verdicts.

## Evaluation checklist

1. **Scope:** target files, exclusions, one task or several?
2. **Acceptance:** measurable completion check?
3. **Constraints:** CLAUDE.md hard rules/guidelines respected?
4. **Security:** auth/tokens, input, filesystem or external APIs? Name the surface.
5. **Testing:** live surface, spanning/regression tests, fixtures? Follow `rules/standards/testing-guidelines.md`.
6. **Context:** missing evidence and reusable prior art?

## Output format

Start with **READY** or **NEEDS CLARIFICATION**, then a **Refined Task Brief** containing:

- **Objective:** one actionable sentence.
- **Scope:** files/modules and what not to touch.
- **Constraints:** relevant CLAUDE.md requirements.
- **Acceptance Criteria:** testable checklist, including live behavior and passing spanning/regression tests.
- **Testing Requirements:** spanning/regression tests.
- **Security Notes:** implications or “None identified”.
- **Questions:** at most three, only for NEEDS CLARIFICATION.

A well-scoped request is READY; never invent problems. A TRIVIAL request gets one line.

## Plan review

Apply `agents/Plan.md` → Plan rules and Phase rules, then check:

1. **Evidence:** rerun 2–3 Target-shape commands. Flag non-reproduction or a `Snapshot:` far behind touched files' HEAD.
2. **Decision:** `DECISION:` rejects a viable alternative, not a straw man.
3. **Baseline:** Phase 0 covers the commands later phases change.
4. **Tasks:** `owns:` names real edited paths for `docs-plan`; external-agent tasks name a `claude` fallback.
5. **Cold start:** spec covered, no scope creep/placeholders, each code change carries its code.

Flag implementation blockers only, not wording, nice-to-haves or extra sections.
Output verdict, then `[Phase X.Y]: issue — why it matters`.
