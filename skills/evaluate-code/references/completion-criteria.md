# Completion criteria and review severity

The checklist every task meets before "done" (from `rules/standards/quality.md`) and this skill's severity
scale. `security-reviewer` keeps its own table (`skills/security-reviewer/SKILL.md` → Severity Levels): same
levels, but a security HIGH is **fix required**, not warn.

## Completion checklist

- [ ] All acceptance criteria satisfied with documented verification
- [ ] Edge cases and error scenarios tested
- [ ] Integration points validated
- [ ] Code review completed (`/code-review` or this skill; `security-reviewer` for auth/input/API/crypto)
- [ ] Automated tests passing; manual testing done for user-facing changes
- [ ] Documentation updated; no broken links or missing references

## Severity

| Level | Meaning | Action | Review-finding label |
|-------|---------|--------|----------------------|
| CRITICAL | Security vulnerability or data-loss risk | **BLOCK** — must fix | must-fix |
| HIGH | Bug or significant quality issue | **WARN** — should fix | should-fix |
| MEDIUM | Maintainability concern | **INFO** — consider | should-fix / nit |
| LOW | Style, minor | **NOTE** — optional | nit |

Approve when no CRITICAL/HIGH. Block on CRITICAL/HIGH.
