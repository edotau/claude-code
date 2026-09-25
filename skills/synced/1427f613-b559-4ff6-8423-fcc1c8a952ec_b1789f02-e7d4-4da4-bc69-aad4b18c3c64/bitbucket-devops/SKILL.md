---
name: bitbucket-devops
description: Bitbucket Data Center (self-hosted, NOT Bitbucket Cloud) repo + pull-request automation via Bearer-auth REST calls, plus Jenkins CI monitoring. For bitbucket.biscrum.com / DIGITALBTD. Trigger on Bitbucket PR, branch, repo, commit, or CI/build status requests.
allowed-tools: Bash, Read, Write, Grep, Glob
---

# Bitbucket DevOps Skill (Data Center + Jenkins)

> **READ THIS FIRST — this instance is Bitbucket DATA CENTER, not Cloud.**
>
> `bitbucket.biscrum.com` is self-hosted Bitbucket **Data Center**: project-scoped
> REST API at `/rest/api/1.0`, **Basic Auth is DISABLED** (Bearer HTTP-access-token
> only), and it has **no Bitbucket Pipelines** (that is a Cloud-only feature).
> CI/CD here runs on **Jenkins** (ODS component pipeline, folder `digitalbtd-cd`).
>
> Consequences for tooling:
> - The bundled `bitbucket-mcp` (Tier 1/2 below) speaks Bitbucket **CLOUD**
>   (`/2.0`, `/repositories/{ws}/...`). Against this instance it returns
>   **404** (wrong paths) — do NOT use it here. It is kept only for reference /
>   any future Cloud workspace.
> - For Bitbucket repo/PR/branch/commit work, use **`lib/bb-dc.sh`** (the
>   Data-Center-native CLI — Bearer auth, correct project-scoped paths).
> - For builds/CI, use the **Jenkins MCP tools** — there is no pipeline API on
>   Bitbucket DC.

## Bitbucket Data Center — use `lib/bb-dc.sh`

Direct Bearer-auth REST calls against `/rest/api/1.0`. Auto-approved (Bash + curl),
no MCP server, no approval prompts.

**Auth/config (all env-overridable, sensible defaults baked in):**
- Token: `$BITBUCKET_TOKEN` → file `$BITBUCKET_TOKEN_FILE` → `~/.ssh/bitbucket-ci-pat`
- Host:  `$BITBUCKET_URL` → `https://bitbucket.biscrum.com/rest/api/1.0`
- Project: `$BITBUCKET_PROJECT` → `DIGITALBTD` (uppercase project KEY — note the git
  remote path `digitalbtd` is lowercase, but the REST API needs the uppercase key)

The token is an HTTP access token (prefix `BBDC…`) minted at
`bitbucket.biscrum.com` → avatar → Manage account → HTTP access tokens. Stored at
`~/.ssh/bitbucket-ci-pat` (chmod 600). Bearer-only because Basic Auth is disabled
server-side.

**Commands:**
```bash
SKILL=~/.claude/skills/bitbucket-devops
bash $SKILL/lib/bb-dc.sh list-repos [project] [limit]
bash $SKILL/lib/bb-dc.sh get-repo  <repo> [project]
bash $SKILL/lib/bb-dc.sh list-branches <repo> [project] [limit]
bash $SKILL/lib/bb-dc.sh list-commits  <repo> [branch] [project] [limit]
bash $SKILL/lib/bb-dc.sh list-prs   <repo> [OPEN|MERGED|DECLINED|ALL] [project] [limit]
bash $SKILL/lib/bb-dc.sh get-pr     <repo> <pr-id> [project]
bash $SKILL/lib/bb-dc.sh pr-diff    <repo> <pr-id> [project]
bash $SKILL/lib/bb-dc.sh pr-activity <repo> <pr-id> [project]
bash $SKILL/lib/bb-dc.sh create-pr  <repo> <from-branch> <to-branch> <title> [desc] [project]
bash $SKILL/lib/bb-dc.sh get        <raw-path>   # escape hatch, e.g. /projects/DIGITALBTD/repos?limit=5
```
This repo's slug is `digitalbtd-portal-databricks`. Example:
```bash
bash ~/.claude/skills/bitbucket-devops/lib/bb-dc.sh list-prs digitalbtd-portal-databricks OPEN
```

## CI / builds — use the Jenkins MCP, not Bitbucket

CI is **Jenkins**: instance `jenkins-digitalbtd-cd`, ODS component pipeline in the
**folder** `digitalbtd-cd`. Buildable jobs are **nested multibranch jobs inside that
folder** (branch→env mapping in `Jenkinsfile`: `dev`→dev, `master`→test).

`jenkins_list_jobs` only shows the top-level folder. To find the real per-branch job,
read the folder's child list from the Jenkins API (the MCP folder/view tools return
the folder; the nested job path looks like `digitalbtd-cd/<component>-<branch>` or
`digitalbtd-cd/<component>/<branch>`). Once you have the job path, use:
`jenkins_get_recent_builds`, `jenkins_get_build_status`, `jenkins_get_console_log`,
`jenkins_get_pipeline_stages`, `jenkins_trigger_build`.

---

## Legacy Cloud tooling (does NOT work here)

`bitbucket-mcp` (the Node.js `lib/helpers.js` + `bitbucket-mcp/dist/index-cli.js` tiers)
targets Bitbucket **Cloud** (`api.bitbucket.org/2.0`) and returns **404** against
`bitbucket.biscrum.com`. For this Data Center instance use `lib/bb-dc.sh` (above) and the
Jenkins MCP for CI. The full Cloud command reference, usage patterns, credential format,
and troubleshooting — if ever needed for a real Cloud workspace — live in
`docs/REFERENCE.md`, `docs/PATTERNS.md`, `docs/GIT_OPERATIONS.md`, and
`docs/TROUBLESHOOTING.md` (plus OpenAPI specs under `docs/bitbucket-api/`).
