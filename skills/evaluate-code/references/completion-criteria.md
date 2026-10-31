# Completion criteria and review severity

The completion checklist every task meets before "done" is canonical in `rules/standards/quality.md` →
Completion. This is this skill's severity scale.

## Severity

| Level | Meaning | Action | Review-finding label |
|-------|---------|--------|----------------------|
| CRITICAL | Security vulnerability or data-loss risk | **BLOCK** — must fix | must-fix |
| HIGH | Bug or significant quality issue | **WARN** — should fix (a security HIGH is **FIX REQUIRED**: `../SKILL.md` → Security) | should-fix |
| MEDIUM | Maintainability concern | **INFO** — consider | should-fix / nit |
| LOW | Style, minor | **NOTE** — optional | nit |

Approve when no CRITICAL/HIGH. Block on CRITICAL/HIGH.
