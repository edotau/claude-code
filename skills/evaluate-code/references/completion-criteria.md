# Completion criteria and review severity

The completion checklist every task meets before "done" is canonical in `rules/standards/quality.md` →
Completion. This is this skill's severity scale.

## Severity

| Level | Meaning | Action | Review-finding label |
|-------|---------|--------|----------------------|
| CRITICAL | Security vulnerability or data-loss risk | **BLOCK** — must fix | must-fix |
| HIGH | Bug or significant quality issue | **FIX REQUIRED** — fix now (security: built-in `/security-review`, `rules/standards/security.md`) | should-fix |
| MEDIUM | Maintainability concern | **FIX NOW** — fixed in the same round, no ask | should-fix |
| LOW | Style, minor | **FIX NOW** — fixed in the same round, LOW included (`rules/standards/quality.md`) | nit |

Every finding, LOW included, is fixed in the same round; approve (SHIP) when no CRITICAL/HIGH is open and MEDIUM is
fixed. Policy: `rules/standards/quality.md` → Completion.
