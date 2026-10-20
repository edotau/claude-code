---
description: "Incident response — triage to SEV1-4, status updates, mitigation timeline, blameless postmortem"
argument-hint: "[new <description> | update <status> | postmortem]"
allowed-tools: "Bash(git log:*), Bash(gh pr list:*), Bash(gh run list:*), Bash(claude-code doctor:*), Bash(claude-code agents tree:*), Bash(claude-code memory path:*), Read, Grep, mcp__claude_ai_Gmail__search_threads, mcp__claude_ai_Gmail__get_thread"
---

# /eng:incident — Incident Response

Run an incident from detection to postmortem. No mode given: ask which phase it is in.

> **Drafts only.** Never page, post to a channel, email, open or transition a ticket, or touch a
> calendar without the user confirming the exact text and recipients. Mitigations that change a
> running system (rollback, flag flip, restart) are proposed, then run only on an explicit yes.

## Task

$ARGUMENTS

## Severity

| Level | Criteria | Response |
|---|---|---|
| SEV1 | Service down, all users affected | Immediate, all hands |
| SEV2 | Major feature degraded, many users | Within 15 min |
| SEV3 | Minor feature issue, some users | Within 1 hour |
| SEV4 | Cosmetic or low impact | Next business day |

## Modes

- **`new`** — classify severity, name affected systems and users, propose roles (incident commander,
  comms, responders). Pull evidence: recent changes (`git log --since=<window>`, `gh pr list --state merged`),
  CI (`gh run list`; Jenkins through the `atlassian` skill), the runbook (Confluence through the
  `atlassian` skill), alert mail (`mcp__claude_ai_Gmail__search_threads`, read only). Incident in the
  harness itself: `claude-code doctor`, `claude-code agents tree`, the bank's `activeContext.md`
  (`claude-code memory path`). Open a UTC timeline in the conversation and keep appending to it.
- **`update`** — draft the status update below. Facts only: what we know, who is affected, what we
  are doing, when the next update lands. No speculation about cause.
- **`postmortem`** — build it from the timeline. Blameless: systems and processes, never people.

```markdown
## Incident Update: <title>
**Severity:** SEV<n> | **Status:** Investigating | Identified | Monitoring | Resolved | **Updated:** <UTC>
**Impact:** <who/what> · **Now:** <what we know> · **Done:** <actions> · **Next:** <action, ETA>
```

Postmortem sections, in order: header (date, duration, severity, authors, status `Draft`) · Summary
(2-3 sentences) · Impact (users, duration, business) · Timeline (UTC table) · Root cause · 5 whys ·
What went well · What went poorly · Action items table (`| Action | Owner | P0-P2 | Due |`).

Adapted from anthropics/knowledge-work-plugins (engineering), Apache-2.0.
