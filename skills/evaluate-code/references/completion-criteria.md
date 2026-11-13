# Completion criteria and review severity

The completion checklist is canonical in `rules/standards/quality.md` → Completion. This is the severity scale.

## Severity

| Level | Meaning | Action | Review-finding label |
|-------|---------|--------|----------------------|
| CRITICAL | Security vulnerability or data-loss risk | **BLOCK** — must fix | must-fix |
| HIGH | Bug or significant quality issue | **FIX REQUIRED** — fix now (security: built-in `/security-review`, `rules/standards/security.md`) | should-fix |
| MEDIUM | Maintainability concern | **FIX NOW** — same round, no ask | should-fix |
| LOW | Style, minor | **FIX NOW** — same round (`rules/standards/quality.md`) | nit |

Every finding, LOW included, is fixed in the same round; SHIP when no CRITICAL/HIGH is open and MEDIUM is fixed.
