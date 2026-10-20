---
description: "Standup update (yesterday / today / blockers) from git, PRs, tickets, calendar and the memory bank"
argument-hint: "[yesterday | today | blockers] [notes…]"
allowed-tools: "Bash(git log:*), Bash(git config:*), Bash(gh search prs:*), Bash(gh pr list:*), Bash(claude-code memory path:*), Bash(claude-code agents tree:*), Bash(claude-code doctor:*), Read, mcp__claude_ai_Google_Calendar__list_events, mcp__claude_ai_Gmail__search_threads"
---

# /eng:standup — Standup Update

Turn recent activity into a three-part standup. Notes in `$ARGUMENTS` beat anything pulled; a mode
(`yesterday`, `today`, `blockers`) prints only that section.

> **Read-only.** Never post, send, comment or transition anything. Sharing the update (chat, email,
> a ticket comment) happens only after the user confirms the exact text and the target.

## Task

$ARGUMENTS

## Gather

Window: since the start of the previous working day (Monday reaches back to Friday). Skip a source
that is unavailable and say which sources ran.

| Source | How |
|---|---|
| Commits | `git log --all --since=<start> --author="$(git config user.email)" --oneline`, per repo in play |
| PRs | `gh search prs --author=@me --updated=">=<start>"` and `gh search prs --reviewed-by=@me --updated=">=<start>"` |
| Jira / Bitbucket / Jenkins | the `atlassian` skill, reads only: tickets moved or commented, PRs, failed builds |
| Calendar | `mcp__claude_ai_Google_Calendar__list_events` for today — meetings that eat focus time |
| Gmail | `mcp__claude_ai_Gmail__search_threads` for unanswered asks addressed to the user — blockers only |
| Memory bank | `claude-code memory path`, then read the top of `sessionHistory.md`, `progress.md`, `activeContext.md` |
| Harness | `claude-code agents tree --running` (in-flight work); `claude-code doctor` ✗ rows count as blockers |

## Output

```markdown
## Standup — YYYY-MM-DD

### Yesterday
- <done item> (<PR #, ticket key or short sha>)

### Today
- <planned item> (<ref>) — from activeContext next steps, open PRs, in-flight agents

### Blockers
- <blocker> — needs <who/what>   (or "None")
```

One line per item, outcome first, no commit-by-commit inventory: group commits under the PR or ticket
they serve. Mark an item inferred only from memory (not from git, PR or ticket) with `(memory)`.

Adapted from anthropics/knowledge-work-plugins (engineering), Apache-2.0.
