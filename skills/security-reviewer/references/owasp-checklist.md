# Mode 1: Application Security Review — OWASP Checklist

The full code-audit checklist. The parent `SKILL.md` carries the severity table, must-review
triggers, and read-only contract; this is the detailed pass to run when auditing application code.

## Pre-Commit Security Checklist

Before ANY commit:

- [ ] No hardcoded secrets (API keys, passwords, tokens)
- [ ] All user inputs validated
- [ ] SQL injection prevention (parameterized queries)
- [ ] XSS prevention (sanitized HTML output)
- [ ] CSRF protection enabled
- [ ] Authentication/authorization verified
- [ ] Rate limiting on endpoints
- [ ] Error messages don't leak sensitive data
- [ ] Logs carry no secrets or PII

## OWASP Top 10

The OWASP Top 10 itself needs no restating here; check each category against the change, then the
harness-specific items above and below. One harness convention worth naming: `%`-style logging args, never
f-strings, so a secret is never interpolated into a log line by accident.

## Response Protocol

If security issue found:

1. STOP immediately; run this skill (`/security-reviewer`) over the affected surface
2. Assess severity (CRITICAL/HIGH/MEDIUM/LOW)
3. Fix CRITICAL issues before any other work
4. Rotate any exposed secret — a committed secret is compromised even after removal; rotate, then scrub git
   history
5. Review the codebase for similar patterns (the `TWINS:` sweep)

(Common false positives to rule out before flagging are listed in the parent `SKILL.md`.)
