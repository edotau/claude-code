# Application security review — OWASP checklist

The detailed pass for auditing application code; `../../SKILL.md` → Security carries the contract. Secrets, input
validation, injection/XSS, CSRF and auth/authz: `rules/standards/security.md`. Check each OWASP Top 10 category
against the change, plus these before ANY commit:

- [ ] Rate limiting on endpoints
- [ ] Error messages don't leak sensitive data
- [ ] Logs carry no secrets or PII (`%`-style logging args, never f-strings)

## Mechanical scan of added lines

```bash
git diff $RANGE | grep -nE '^\+' | grep -iE 'password|secret|api[_-]?key|token|BEGIN (RSA|PRIVATE)|eval\(|exec\(|pickle\.loads|md5|sha1|verify=False|shell=True'
```

## Response Protocol

If a security issue is found:

1. STOP immediately; run the Security lens (`evaluate-code`) over the affected surface
2. Assess severity (CRITICAL/HIGH/MEDIUM/LOW)
3. Fix CRITICAL issues before any other work
4. Rotate any exposed secret — a committed secret is compromised even after removal; rotate, then scrub git
   history
5. Review the codebase for similar patterns (the problem-solving `ISSUE:` sweep)

## Common False Positives

Verify context before flagging:

- Environment variables in `.env.example` (not actual secrets)
- Test credentials in test files (if clearly marked)
- Public API keys (if actually meant to be public)
- SHA256/MD5 used for checksums (not passwords)
