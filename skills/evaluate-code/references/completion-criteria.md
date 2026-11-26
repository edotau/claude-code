# Completion criteria and review severity

Checklist and review loop: `rules/standards/quality.md` → Completion.

## Severity

- **CRITICAL:** security vulnerability/data-loss risk → **BLOCK**, must-fix.
- **HIGH:** bug/significant quality issue → **FIX REQUIRED**, should-fix; security uses built-in `/security-review`.
- **MEDIUM:** maintainability concern → **FIX NOW**, should-fix, same round without asking.
- **LOW:** style/minor → **FIX NOW**, nit, same round without asking (R28).
