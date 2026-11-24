# Security Standards

Language specifics: `rules/software/<lang>/security.md`, where one exists (Go: `rules/harness/go.md`).
Harness credentials: CLAUDE.md hard rule 2.

1. **No secrets in code** — env vars or a secret manager; `.env`, `credentials.json`, `*.pem` stay out of git
   (`claude-code doctor --scan [path…]` flags inline secrets in configs and the vault; `doctor --git` scans history
   for committed ones and real workspace hosts, line by line, so a concatenated host passes). A secret in a tracked
   file is urgent: rotate it, use `${ENV_VAR}`, scrub history if pushed.
2. **Validate input** — sanitize file paths (no traversal); subprocess with list args, never a concatenated shell
   string; parameterized queries.
3. **Dependencies** — standard library first; minimal, pinned, audited. A Pi package's extension code runs unsandboxed
   in Pi with your privileges and the guard does not judge it: approve a package only after reading its code.
4. **Production changes go through the deploy pipeline** — a main session asks before `kubectl|oc exec|delete`
   or `oc rsh` against a production cluster (`safety-scan`, `rules/harness/hooks.md`); a dispatched leaf may run
   them with a note, since `agentBashAllow` lifts that ask. Read logs and status, copy artifacts out
   (`kubectl cp`/`oc cp`), never decode a cluster secret, and keep production changes in the deploy pipeline.
5. **Browser automation on authorized sites only** — ours, internal, or with explicit permission; honor robots.txt
   (`network websearch --extract` checks it); no detection evasion (fingerprint spoofing, webdriver masking, anti-bot
   proxy rotation) — a block means ask for an API or permission.
6. **Public repo hygiene** — check license and branding before tracking an asset; no private host names, LAN or tailnet
   addresses in any tracked file or commit (role names only; public documentation URLs are fine).
