---
description: "Pre-deploy checklist for a release — CI, approvals, migrations, flags, rollback triggers decided up front"
argument-hint: "[service or release] [migration] [flags] [breaking]"
allowed-tools: "Bash(git log:*), Bash(git describe:*), Bash(git diff:*), Bash(gh pr list:*), Bash(gh pr checks:*), Bash(gh run list:*), Bash(claude-code doctor:*), Bash(claude-code agents tree:*), Read"
---

# /eng:deploy-checklist — Deploy Checklist

Build the checklist for one release and pre-tick only what evidence proves. Anything not checked stays
`[ ]` with `unverified`.

> **Checks only.** Never deploy, merge, tag, push, notify or close a ticket. Every outward step in the
> checklist is the user's to run, or runs only after they confirm it.

## Task

$ARGUMENTS

## Gather

- Release diff: `git log $(git describe --tags --abbrev=0)..HEAD --oneline` (no tag: ask for the base).
- Approvals: `gh pr list --state merged --base <branch> --search "merged:>=<base date>"`; `gh pr checks <n>`.
- CI: `gh run list --branch <branch> --limit 5`; Jenkins builds and the release's Jira tickets through
  the `atlassian` skill (reads only).
- Releasing this harness: `make test` green, `claude-code doctor` clean, `claude-code secrets git` clean,
  `claude-code agents tree --running` empty (no agent still writing the tree).

## Output

```markdown
## Deploy Checklist: <service/release> — YYYY-MM-DD — deployer: <name>

### Pre-deploy
- [ ] CI green on the release commit
- [ ] Every PR reviewed and approved
- [ ] No known critical bug in the release
- [ ] Rollback plan written
- [ ] On-call told (user sends)

### Deploy
- [ ] Staging deployed and verified, smoke tests pass
- [ ] Production (canary first if available)
- [ ] Error rate and latency watched 15 min; key user flows verified

### Post-deploy
- [ ] Metrics nominal; changelog updated
- [ ] Stakeholders told (user sends); tickets closed

### Rollback triggers (decided now, not during)
- Error rate > <X>% · p50 latency > <X> ms · <critical flow> fails
```

Arguments add sections: `migration` → migration tested on a prod-size copy, reversible or backed up;
`flags` → flag defaults and kill switch verified; `breaking` → consumers notified, versioned endpoint
kept. Take rollback thresholds from the user or a monitoring baseline; never invent numbers.

Adapted from anthropics/knowledge-work-plugins (engineering), Apache-2.0.
