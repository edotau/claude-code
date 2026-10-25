# Mode 1: Application Security Review — OWASP Checklist

The full code-audit checklist. `../../SKILL.md` → Security carries the contract, must-review
triggers, and read-only contract; this is the detailed pass to run when auditing application code.

## Mechanical scan of added lines

```bash
git diff $RANGE | grep -nE '^\+' | grep -iE 'password|secret|api[_-]?key|token|BEGIN (RSA|PRIVATE)|eval\(|exec\(|pickle\.loads|md5|sha1|verify=False|shell=True'
```

## Pre-Commit Security Checklist

Secrets, input validation, injection/XSS, CSRF, auth/authz: `rules/standards/security.md` →
Before commit. This skill adds, before ANY commit:

- [ ] Rate limiting on endpoints
- [ ] Error messages don't leak sensitive data
- [ ] Logs carry no secrets or PII

## OWASP Top 10

The OWASP Top 10 itself needs no restating here; check each category against the change, then the
harness-specific items above and below. One harness convention worth naming: `%`-style logging args, never
f-strings, so a secret is never interpolated into a log line by accident.

## Response Protocol

If security issue found:

1. STOP immediately; run the Security lens (`evaluate-code`) over the affected surface
2. Assess severity (CRITICAL/HIGH/MEDIUM/LOW)
3. Fix CRITICAL issues before any other work
4. Rotate any exposed secret — a committed secret is compromised even after removal; rotate, then scrub git
   history
5. Review the codebase for similar patterns (the problem-solving `ISSUE:` sweep)

## Common False Positives

Always verify context before flagging:

- Environment variables in `.env.example` (not actual secrets)
- Test credentials in test files (if clearly marked)
- Public API keys (if actually meant to be public)
- SHA256/MD5 used for checksums (not passwords)
