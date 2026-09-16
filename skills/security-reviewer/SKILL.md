---
name: security-reviewer
description: 'Use when reviewing code or a diff for security: secrets, injection, SSRF, auth/authorization, unsafe crypto, OWASP Top 10, auditing Claude Code config (settings.json, hooks, MCP), or auditing project dependencies for CVEs and license compliance. Triggers "security review", "review for vulnerabilities", "is this safe", "/security", "check for secrets", "OWASP", "audit dependencies", "any CVEs in our deps", "vulnerable packages", "license compliance".'

metadata:
  version: 2.0.0
---

# Security Reviewer

The security half of the review stack. The `evaluate-code` skill owns code *quality* and the
four-principle discipline lens; this skill owns the *security* pass they run in parallel.
Loaded by the main session or any subagent (there is no dedicated security-reviewer agent —
this skill is self-contained), and surfaced by the `/security` command.

Three modes: application security review (OWASP-style code audit), configuration security
scanning (Claude Code harness audit), and dependency auditing (CVEs + license compliance).

## Review contract (read-only by convention)

Propose fixes in findings; **never patch auth/crypto code directly** — a "helpful" auto-edit in
security-sensitive code is itself a risk. This is a *convention* here (a skill can't restrict
tools the way the retired agent's `readonly: true` did), so the loading session must honor it:
report findings, let the human or a write-capable agent apply them. When run as `/security`, keep
the pass read-only and end with a PASS / FIX REQUIRED / BLOCK verdict.

## Severity Levels

| Level | Meaning | Action |
|-------|---------|--------|
| CRITICAL | Exploitable vulnerability or data-loss risk | **BLOCK** — must fix before merge |
| HIGH | Likely-exploitable or significant exposure | **FIX REQUIRED** — fix before merge |
| MEDIUM | Hardening gap, defense-in-depth | **INFO** — consider |
| LOW | Minor / informational | **NOTE** — optional |

## Must-Review Triggers

STOP and run a full pass when a change touches: authentication / authorization, user-input
handling, database queries, file-system operations, external API calls, cryptography, or
payment code. These are non-negotiable review territory.

## Mode 1: Application Security Review

Full OWASP Top 10 code-audit checklist + pre-commit list + response protocol + common
false positives — **read [references/owasp-checklist.md](references/owasp-checklist.md)**.

## Mode 2: Configuration Security Scan

Scan Claude Code config (CLAUDE.md/settings.json/MCP/hooks/agents) for injection, dangerous
permissions, and exposed secrets — scan commands, severity grades, common config issues in
**[references/agentshield-config-scan.md](references/agentshield-config-scan.md)**. For cloud /
IaC / IAM / CI-CD review, see `cloud-infrastructure-security.md` in this skill directory.

## Mode 3: Dependency Audit

Audit project dependencies for known vulnerabilities (CVEs), license compliance, and safe
upgrade planning — driven by **live-feed scanners** (`osv-scanner`, `pip-audit`, `npm audit`),
never a hardcoded CVE list. Trigger on "audit dependencies", "any CVEs in our deps", "vulnerable
packages", or license-compliance questions. (Auditing *existing* deps here; the question of
*whether* to add a new dependency at all — prefer a maintained library over custom code — is a
design-time call.)

**Full procedure — read `references/dependency-auditor.md`.** License compatibility grid in
`references/dependency-license-matrix.md`.

Core rule: a scanner that isn't installed reports **UNKNOWN**, never "clean". `npm` is the only
scanner present by default here — probe with `command -v`, install on demand
(`pipx install pip-audit osv-scanner`), or state the gap.

**Scoped to read-only scanners ONLY** (preserves the read-only contract): `osv-scanner`,
`pip-audit` (WITHOUT `--fix`), `npm audit` (WITHOUT `fix`), `pip-licenses`, `pip list --outdated`,
`npm outdated`. Never run `--fix` / `audit fix`, project-mutating installs, or edits — propose
upgrades as findings; the user or a write-capable agent applies them.

## Common False Positives

Always verify context before flagging:
- Environment variables in `.env.example` (not actual secrets)
- Test credentials in test files (if clearly marked)
- Public API keys (if actually meant to be public)
- SHA256/MD5 used for checksums (not passwords)
