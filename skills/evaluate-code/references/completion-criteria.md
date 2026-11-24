# Completion criteria and review severity

The completion checklist is canonical in `rules/standards/quality.md` → Completion. This is the severity scale.

## Severity

| Level | Meaning | Action | Review-finding label |
|-------|---------|--------|----------------------|
| CRITICAL | Security vulnerability or data-loss risk | **BLOCK** — must fix | must-fix |
| HIGH | Bug or significant quality issue | **FIX REQUIRED** — fix now (security: built-in `/security-review`, `rules/standards/security.md`) | should-fix |
| MEDIUM | Maintainability concern | **FIX NOW** — same round, no ask | should-fix |
| LOW | Style, minor | Fix in the loop; at most 4 may remain (R28, `rules/standards/quality.md`) | nit |

Fix and re-review until no CRITICAL, HIGH or MEDIUM is open and at most 4 LOW remain (none is better); then SHIP (R28).
