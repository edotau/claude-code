# Security Standards

Language specifics: `rules/software/<lang>/security.md`. Harness credentials live only in the encrypted vault
(`claude-code vault set`) or a provider's `auth.command` (CLAUDE.md hard rule 2; `README.md` → Credentials).

## Core Principles

1. **No secrets in code** — env vars or a secret manager; exclude `.env`, `credentials.json`, `*.pem` from git;
   validate required secrets at startup; scan before committing (`claude-code doctor --scan`). A secret in a
   git-tracked file is urgent: rotate it at the source, replace it with `${ENV_VAR}`, scrub history if pushed.
2. **Input validation** — validate all user input; sanitize file paths (no traversal); subprocess with list
   args, never shell-string concatenation.
3. **Dependency security** — standard library first; minimal, pinned, audited external deps.
4. **Data protection** — collect minimal user data; encrypt in transit; GDPR/CCPA where applicable.
5. **Production is read-only from a session** — never `exec`/shell into a live pod or decode a cluster
   secret; read logs and status, copy artifacts out (`kubectl cp`/`oc cp`), change production only through
   its deploy pipeline.

**Before commit:** no hardcoded secrets and none leaked in logs or errors; inputs validated; parameterized
queries, sanitized output, CSRF on web; authentication/authorization verified on protected routes; deps pinned
and audited.

**Must-review:** auth, input, database, file-system, external-API, crypto and payment code gets the
**evaluate-code** Security lens (`skills/evaluate-code/SKILL.md` → Security) before merge; a found issue follows its
response protocol (`skills/evaluate-code/references/security/owasp-checklist.md` → Response Protocol).
