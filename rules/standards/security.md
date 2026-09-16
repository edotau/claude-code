# Security Standards

Language specifics: `rules/software/<lang>/security.md`. Harness credentials live only in
`~/.claude/env.d/secrets.env` or a provider's `auth.command` (see `README.md`).

## Core Principles

1. **No secrets in code** — never hardcode API keys, passwords, or tokens. Use env vars
   or a secret manager. Exclude `.env`, `credentials.json`, `*.pem` from git. Scan before
   committing. Validate required secrets are present at startup.
2. **Input validation** — validate all user input; sanitize file paths (prevent
   traversal); prevent command injection (subprocess with list args, never shell-string
   concatenation); handle edge cases safely.
3. **Dependency security** — prefer the standard library; minimize and pin external
   deps; audit for known vulnerabilities; keep updated.
4. **Data protection** — collect minimal user data; encrypt in transit; follow privacy
   law (GDPR/CCPA) where applicable.

## Mandatory Pre-Commit Checklist

- [ ] No hardcoded secrets (API keys, passwords, tokens)
- [ ] All user inputs validated
- [ ] SQL injection prevention (parameterized queries)
- [ ] XSS prevention (sanitized output)
- [ ] CSRF protection enabled (web)
- [ ] Authentication / authorization verified on protected routes
- [ ] No secrets leaked in logs or error messages
- [ ] Dependencies pinned + audited

## Prohibited vs Correct

```python
# WRONG — hardcoded secret
API_KEY = "sk-abc123..."

# CORRECT — from environment
API_KEY = os.environ["API_KEY"]      # raises if missing
```

```python
# WRONG — shell injection
os.system(f"git clone {url}")

# CORRECT — list args, no shell
subprocess.run(["git", "clone", url], check=True)
```

## Security Response Protocol

If a security issue is found:

1. **STOP** immediately.
2. Run the **security-reviewer** skill (via `/security`).
3. Fix CRITICAL issues before continuing anything else.
4. **Rotate any exposed secret** — assume a committed secret is compromised even after
   removal; rotate the credential, then scrub git history.
5. Review the codebase for similar instances.

Auth/input/database/file-system/external-API/crypto/payment code is must-review
territory — run the **security-reviewer** skill (via `/security`) before merge.
