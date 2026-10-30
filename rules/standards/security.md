# Security Standards

Language specifics: `rules/software/<lang>/security.md`. Harness credentials: CLAUDE.md hard rule 2.

1. **No secrets in code** — env vars or a secret manager; `.env`, `credentials.json`, `*.pem` stay out of git; scan
   before committing (`claude-code doctor --scan`). A secret in a tracked file is urgent: rotate it at the source,
   replace it with `${ENV_VAR}`, scrub history if pushed.
2. **Validate input** — sanitize file paths (no traversal); subprocess with list args, never a concatenated shell
   string; parameterized queries.
3. **Dependencies** — standard library first; minimal, pinned, audited.
4. **Production is read-only from a session** — never `exec`/shell into a live pod or decode a cluster secret; read
   logs and status, copy artifacts out (`kubectl cp`/`oc cp`), change production only through its deploy pipeline.
5. **Browser automation on authorized sites only** — ours, internal, or with explicit permission; honor robots.txt
   (`network browser --extract` checks it); no detection evasion (fingerprint spoofing, webdriver masking, anti-bot
   proxy rotation) — a block means ask for an API or permission.

Auth, input, database, file-system, external-API, crypto and payment code gets the **evaluate-code** Security lens
before merge; a found issue follows `skills/evaluate-code/references/security/owasp-checklist.md` → Response Protocol.
