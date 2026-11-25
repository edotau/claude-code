# Completion criteria and review severity

The completion checklist and the review loop (R28) are canonical in `rules/standards/quality.md` → Completion. This is
the severity scale.

## Severity

| Level | Meaning | Action | Review-finding label |
|-------|---------|--------|----------------------|
| CRITICAL | Security vulnerability or data-loss risk | **BLOCK** — must fix | must-fix |
| HIGH | Bug or significant quality issue | **FIX REQUIRED** — fix now (security: built-in `/security-review`) | should-fix |
| MEDIUM | Maintainability concern | **FIX NOW** — same round, no ask | should-fix |
| LOW | Style, minor | **FIX NOW** — same round, no ask (R28, `rules/standards/quality.md`) | nit |
