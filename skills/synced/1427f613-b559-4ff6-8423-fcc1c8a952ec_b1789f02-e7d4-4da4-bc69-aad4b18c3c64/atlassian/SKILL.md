---
name: atlassian
description: Self-hosted Atlassian Data Center suite (NOT Cloud) on *.biscrum.com — Bitbucket repos/PRs via Bearer REST, Jira issue tracking (JQL), Confluence pages/runbooks (CQL), plus the Jenkins CI that replaces Bitbucket Pipelines. Trigger on Bitbucket PR/branch/repo/commit, build/CI status, Jira ticket/JQL/sprint, or Confluence page/space requests.
allowed-tools: Bash, Read, Write, Grep, Glob
---

# Atlassian Data Center Skill

Three self-hosted Atlassian products on `*.biscrum.com`, plus the Jenkins CI they run on.

| Surface | Host | Use | Tools |
|---|---|---|---|
| **Bitbucket** | `bitbucket.biscrum.com` | repos, PRs, branches, commits, diffs | `mcp__bitbucket-dc__*` (or `lib/bb-dc.sh`) |
| **Jira** | `jira.biscrum.com` | issues, JQL, transitions | `mcp__atlassian__jira_*` |
| **Confluence** | `confluence.biscrum.com` | pages, CQL, runbooks | `mcp__atlassian__confluence_*` |
| **Jenkins** (CI) | `jenkins-digitalbtd-cd` | builds/pipeline status | `mcp__jenkins__jenkins_*` |

> **Two invariants across all three Atlassian surfaces:**
> 1. **Data Center, not Cloud.** Cloud is NOT supported anywhere here — different REST path
>    shape and auth. Bitbucket has **no Pipelines** (Cloud-only); CI is Jenkins.
> 2. **Per-app Bearer PAT.** DC issues tokens per application, so a Bitbucket, Jira, and
>    Confluence PAT are three distinct tokens. Each resolves env → chmod-600 file. A `401`
>    from a tool means that app's token isn't set — mint one (app → profile → Personal Access
>    Tokens) and drop it in the file. Basic Auth is disabled server-side; Bearer only.

Default project key: **`DIGITALBTD`** (uppercase for the REST API; the git remote path
`digitalbtd` is lowercase). This repo's slug: `digitalbtd-portal-databricks`.

## Bitbucket — `bitbucket-dc` MCP (or `lib/bb-dc.sh` fallback)

Prefer the **`bitbucket-dc` MCP** (`mcp__bitbucket-dc__*`, 27 tools): PRs
(list/get/create/merge/approve/decline), inline + editable comments, reviews, diffs, code
insights, dashboards, branches, commits, file content, repo browsing. It's the garc33
Bitbucket-Server MCP nested at `plugin/`, launched via `~/.claude/mcp/launch-bitbucket-mcp.sh`
(reads the Bearer token, normalizes `BITBUCKET_URL` to the bare host — garc33 appends
`/rest/api/1.0` itself).

`lib/bb-dc.sh` is a **zero-dependency curl fallback** (same Bearer auth / `/rest/api/1.0` /
project defaults) for quick shell calls or when the MCP is down — auto-approved, no prompts:

```bash
SKILL=~/.claude/skills/atlassian
bash $SKILL/lib/bb-dc.sh list-prs   <repo> [OPEN|MERGED|DECLINED|ALL] [project] [limit]
bash $SKILL/lib/bb-dc.sh get-pr     <repo> <pr-id> [project]
bash $SKILL/lib/bb-dc.sh pr-diff    <repo> <pr-id> [project]
bash $SKILL/lib/bb-dc.sh pr-activity <repo> <pr-id> [project]
bash $SKILL/lib/bb-dc.sh create-pr  <repo> <from-branch> <to-branch> <title> [desc] [project]
bash $SKILL/lib/bb-dc.sh list-repos|get-repo|list-branches|list-commits ...
bash $SKILL/lib/bb-dc.sh get <raw-path>   # escape hatch, e.g. /projects/DIGITALBTD/repos?limit=5
```

**Auth/config** (env-overridable): token `$BITBUCKET_TOKEN` → `$BITBUCKET_TOKEN_FILE` →
`~/.ssh/bitbucket-ci-pat` (HTTP access token, prefix `BBDC…`); host `$BITBUCKET_URL` →
`https://bitbucket.biscrum.com`; project `$BITBUCKET_PROJECT` → `DIGITALBTD`.

## Jira & Confluence — `atlassian` MCP

Jira at `jira.biscrum.com/rest/api/2`, Confluence at `confluence.biscrum.com/rest/api`. The
`atlassian` MCP server is already registered in `.mcp.json` and enabled — no setup beyond the
per-app token:
- Jira: `$JIRA_TOKEN` / `$ATLASSIAN_TOKEN` → `~/.ssh/atlassian-ci-pat`
- Confluence: `$CONFLUENCE_TOKEN` / `$ATLASSIAN_TOKEN` → `~/.ssh/confluence-ci-pat` → shared `~/.ssh/atlassian-ci-pat`

**Jira** (`mcp__atlassian__*`): `jira_search` (JQL, e.g. `project = DIGITALBTD AND status = "In Progress"`),
`jira_get_issue` (by key), `jira_create_issue`, `jira_add_comment`,
`jira_list_transitions` / `jira_transition_issue`, `jira_raw_get` (GET any `/rest/api/2/...`).

**Confluence** (`mcp__atlassian__*`): `confluence_search` (CQL, e.g. `space = DIGITALBTD AND type = page`),
`confluence_get_page` (expands body/version/space), `confluence_create_page` (XHTML storage
format, optional `parent_id`), `confluence_raw_get` (GET any `/rest/api/...`).

## Jenkins CI (Bitbucket DC has no Pipelines)

Instance `jenkins-digitalbtd-cd`; ODS component pipeline in the **folder** `digitalbtd-cd`.
Buildable jobs are **nested multibranch jobs inside that folder** (branch→env in `Jenkinsfile`:
`dev`→dev, `master`→test). `jenkins_list_jobs` shows only the top-level folder — read its child
list to find the per-branch job (`digitalbtd-cd/<component>-<branch>` or
`.../<component>/<branch>`), then `jenkins_get_recent_builds`, `jenkins_get_build_status`,
`jenkins_get_console_log`, `jenkins_get_pipeline_stages`, `jenkins_trigger_build`.

## Cross-surface debugging (a broken build)

Correlate the four surfaces instead of guessing:

1. **Failure** — `jenkins_get_console_log` / `jenkins_get_pipeline_stages` on the per-branch
   job to find the failing stage.
2. **Change** — `mcp__bitbucket-dc__list_prs` → `pr_diff` / `pr_activity` for the offending diff
   and reviewer comments.
3. **Ticket** — `jira_search` keyed off the branch/commit ticket (`key = DIGITALBTD-123`), then
   `jira_get_issue` to read context, `jira_add_comment` / `jira_transition_issue` to record it.
4. **Runbook** — `confluence_search` (CQL) → `confluence_get_page` for the component's
   known-issue page; `confluence_create_page` to capture a postmortem when the fix is non-obvious.

## Layout

```
atlassian/
├── SKILL.md                 # this file
├── lib/bb-dc.sh             # zero-dep curl fallback (Bitbucket, Bearer, /rest/api/1.0)
├── plugin/                  # garc33 bitbucket-server MCP (self-contained: build/ + node_modules/)
│   └── build/index.js       # launched by ~/.claude/mcp/launch-bitbucket-mcp.sh
└── docs/                    # Bitbucket CLOUD API reference — retained for reference only,
                             #   does NOT apply to this Data Center instance
```
