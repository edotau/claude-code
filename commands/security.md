---
description: "Security review of the current diff — secrets, injection, auth, OWASP Top 10 — via the security-reviewer skill"
argument-hint: "[diff range, or blank for staged/branch changes]"
allowed-tools: Bash, Read, Grep, Glob, Agent
---

Run a focused security pass over the changes. This is the security half of the review
stack — pair it with `/code-review` (quality). Load the `security-reviewer` skill
(`~/.claude/skills/security-reviewer/SKILL.md`) for the OWASP checklist, severity rubric, and
read-only contract, and apply it directly in this session.

## 1. Determine diff scope

```bash
# Explicit range wins
RANGE="$ARGUMENTS"
# Else: feature branch vs main
[ -z "$RANGE" ] && BASE=$(git merge-base HEAD main 2>/dev/null) && RANGE="${BASE}...HEAD"
# Else: staged changes
git diff ${RANGE:---cached} --stat
git diff --name-only ${RANGE:---cached}
```
If there is no branch divergence and nothing staged, fall back to `git diff --cached` and
say so.

## 2. Fast mechanical scan

Before dispatching, grep the added lines for the obvious tells (cite file:line):

```bash
git diff ${RANGE:---cached} | grep -nE '^\+' | \
  grep -iE 'password|secret|api[_-]?key|token|BEGIN (RSA|PRIVATE)|eval\(|exec\(|pickle\.loads|md5|sha1|verify=False|shell=True'
```

## 3. Run the security-reviewer skill

Load `~/.claude/skills/security-reviewer/SKILL.md` and apply Mode 1 over the scoped diff —
read the full changed files for context, not just the hunks. The OWASP checklist, the severity
rubric and the read-only contract are the skill's; report each finding at its severity with
file:line and a concrete fix.

For a large diff (many files), dispatch a **general-purpose** subagent that loads the
`security-reviewer` skill and returns findings, so the pass runs with a clean context — but keep
it read-only (findings only).

If the change touches Claude Code config (`settings.json`, hooks, MCP, agents), also run
Mode 2 (config scan) from the skill. If it touches dependency manifests, also run Mode 3
(dependency audit).

## 4. Verdict

End with the severity-count table and the skill's PASS / FIX REQUIRED / BLOCK verdict (its
"Severity Levels" section). If anything is BLOCK/FIX REQUIRED, list
the must-fixes first with file:line and the remediation.
