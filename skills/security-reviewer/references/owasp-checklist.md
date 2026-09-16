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

## OWASP Top 10 Review Areas

**1. Secrets Management**
- Scan for hardcoded credentials: `grep -rn "password\|api_key\|secret\|token" --include="*.py" --include="*.ts" --include="*.js"`
- Verify `.env` files are in `.gitignore`
- Check that secrets load from environment variables or secret managers

**2. Input Validation**
- All user input validated at system boundaries
- Schema-based validation where available (Pydantic, Zod, JSON Schema)
- Fail fast with clear error messages
- Never trust external data

**3. Injection Prevention**
- SQL: parameterized queries only, never string concatenation
- Command injection: no unsanitized user input in shell commands
- Path traversal: validate and sanitize file paths

**4. Authentication & Authorization**
- Session management: secure cookies, proper expiry
- Password handling: bcrypt/argon2, never plain text
- Role-based access control verified at every endpoint
- JWT validation: check signature, expiry, issuer

**5. XSS Prevention**
- HTML output escaped/sanitized
- Content-Security-Policy headers set
- No `innerHTML` with user data

**6. CSRF Protection**
- Anti-CSRF tokens on state-changing requests
- SameSite cookie attribute set

**7. Rate Limiting**
- All public endpoints rate-limited
- Authentication endpoints especially protected
- Configurable limits per endpoint

**8. Sensitive Data Exposure**
- Error messages don't reveal stack traces or internal paths
- Logging doesn't include secrets or PII
- %-style logging args, NOT f-strings (prevents accidental interpolation)

**9. Dependency Security**
- `pip audit` / `npm audit` / `cargo audit` for known vulnerabilities
- Pin dependency versions
- Review transitive dependencies

**10. Configuration Security**
- Debug mode disabled in production
- Default credentials changed
- Unnecessary features/ports disabled

## Response Protocol

If security issue found:
1. STOP immediately
2. Assess severity (CRITICAL/HIGH/MEDIUM/LOW)
3. Fix CRITICAL issues before any other work
4. Rotate any exposed secrets
5. Review codebase for similar patterns

(Common false positives to rule out before flagging are listed in the parent `SKILL.md`.)
