# Completion criteria and review severity

The completion checklist is canonical in `rules/standards/quality.md` → Completion. This is the severity scale.

## Severity

| Level | Meaning | Action | Review-finding label |
|-------|---------|--------|----------------------|
| CRITICAL | Security vulnerability or data-loss risk | **BLOCK** — must fix | must-fix |
| HIGH | Bug or significant quality issue | **FIX REQUIRED** — fix now (security: built-in `/security-review`) | should-fix |
| MEDIUM | Maintainability concern | **FIX NOW** — same round, no ask | should-fix |
| LOW | Style, minor | **FIX NOW** — same round, no ask (R28, `rules/standards/quality.md`) | nit |

Review, fix every finding without asking, review again until one finds nothing (three rounds in a row with a new HIGH
in one area: stop, bring the pattern to the owner); SHIP when the last shows no CRITICAL or HIGH (R28).
