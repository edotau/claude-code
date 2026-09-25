---
name: databricks-expert
description: Use for Databricks work — auth, profiles, tokens, databrickscfg; WorkspaceClient / Python SDK; the databricks CLI; SQL warehouses and queries; Unity Catalog, volumes, system tables; Jobs and cron/triggers; Asset Bundles (DABs) deploy/validate/run; plus coordinated harness-SDK / Databricks-SDK code changes and repo restructures (see references/harness-sdk-changes.md). Includes DigitalBTD dev access — repo-root .databrickscfg, sp-dev service principal, the digitalbtd-dev/qa/prod workspaces and the insilico-dev repo venv (see rules/digitalbtd-databricks.md). Triggers include databricks, databricks-expert, databrickscfg, WorkspaceClient, bundle deploy, dbx, job/cron, UC volume, serverless, sp-dev, digitalbtd, insilico, mmseqs, Unity Catalog volume, SQL warehouse.
---

# Databricks

Unified entry point for Databricks auth, SDK, CLI, SQL/Unity Catalog, Jobs, and Asset
Bundles. This core covers the common path; load a `references/` file when a task needs
depth.

> **Working in a DigitalBTD repo?** Read `rules/digitalbtd-databricks.md` **before the first
> `WorkspaceClient` call** — it is a five-step preflight (repo-root config file, `sp-dev`,
> host allowlist, backticked catalog, repo venv) that prevents the two failures which look like
> credential errors but are not. `dbx_client()` in `kernel.py` applies steps 1-2 for you.

## References

| Need | Read |
|------|------|
| Onboarding: zero → authenticated (install → OAuth → SP creds), ordered | `references/setup.md` |
| DigitalBTD dev access: repo venv, repo-root `.databrickscfg`, `sp-dev`, 3 workspaces, read-only rule | `references/digitalbtd-dev.md` |
| Coordinated code changes: harness SDK / `WorkspaceClient` usage / repo restructures + guardrails | `references/harness-sdk-changes.md` |
| Auth, profiles, tokens, U2M/M2M, the Claude Code shell gotcha | `references/auth-profiles.md` |
| CLI install, command quick-ref, AI data-exploration tools | `references/cli.md` |
| Python SDK (`WorkspaceClient`) surfaces, errors, async | `references/sdk.md` |
| Jobs: task types, triggers, compute, params, permissions | `references/jobs.md` |
| Job notifications, health rules, timeouts, retries | `references/jobs-monitoring.md` |
| Copy-paste job archetypes (ETL, ML, event-driven, multi-env) | `references/jobs-examples.md` |
| Asset Bundles (DABs): structure, variables, targets, deploy | `references/asset-bundles.md` |
| Unity Catalog: volumes, MCP tools, system tables | `references/unity-catalog.md` |
| Lakeflow Declarative Pipelines (DLT): @dp.table, expectations, deploy | `references/pipelines.md` |
| AI Functions: `ai_classify/extract/summarize/query`, batch inference | `references/ai-functions.md` |
| Vector Search: endpoints, DELTA_SYNC/DIRECT indexes, embeddings, query | `references/vector-search.md` |
| Model Serving: endpoint lifecycle, UC-model serving, querying | `references/model-serving.md` |
| MLflow: tracking, UC model registry, GenAI `evaluate()` + scorers | `references/mlflow.md` |
| Agent Bricks (KA/MAS) + Genie spaces: `manage_ka/mas/genie`, `ask_genie` | `references/agent-bricks.md` |
| DBSQL: SQL warehouses, statement execution, query patterns | `references/dbsql.md` |
| Execute code on serverless/classic/interactive compute; `manage_cluster` | `references/execution-compute.md` |
| Notebook lifecycle: list/create/run/download/diff/sync (import/export, `execute_code`) | `references/notebooks.md` |
| Databricks Apps: build/deploy data apps (AppKit vs Python), `manage_app` | `references/apps.md` |
| Iceberg tables: managed, external reads, compatibility mode | `references/iceberg.md` |
| Spark Structured Streaming: readStream/writeStream, checkpoints, triggers | `references/streaming.md` |
| Lakebase Postgres: projects, synced tables, Data API, `manage_lakebase_*` | `references/lakebase.md` |
| Serverless migration: moving classic-compute workloads, unsupported-feature checklist | `references/serverless-migration.md` |
| Lakeflow Connect: managed SaaS/DB ingestion pipelines | `references/lakeflow-connect.md` |
| Zerobus: near-real-time gRPC ingest into Delta | `references/zerobus.md` |
| PABLO/portal-databricks worked patterns (var injection, for-each, SP auth) | `references/portal-databricks.md` |
| `CLAUDE_CODE_DBX_*` private namespace design (harness auth decouple) | `references/env-decouple.md` |
| Harness auth SDK: PAT/U2M ladder, `token_store`/`token_display` CLIs, `AuthTokenManager` | `references/harness-auth.md` |
| Model routing: the two "Opus 4.8" names, drift guard, per-workspace pinning | `references/model-routing.md` |

## Auth (essentials)

SDK/CLI resolve credentials first-match-wins: explicit token → `DATABRICKS_CLIENT_ID`+
`DATABRICKS_CLIENT_SECRET` (M2M) → `DATABRICKS_TOKEN` → `~/.databrickscfg` profile
(`DATABRICKS_CONFIG_PROFILE`). Setting a token **and** client_id/secret together errors
(`more than one authorization method`).

**The profile file is not always `~/.databrickscfg`.** The last rung resolves
`$DATABRICKS_CONFIG_FILE` first and only then `~/.databrickscfg`. A repo that keeps its own
config at the repo root (see "DigitalBTD repos (dev access)" below) therefore looks like a
*credential* failure when it is really a *path* failure: `WorkspaceClient(profile="X")` raises
`default auth: cannot configure default credentials` even though the profile exists and is
valid. Set the path before the first client call:

```python
import os
os.environ["DATABRICKS_CONFIG_FILE"] = "<repo>/.databrickscfg"   # before WorkspaceClient(...)
```

**Claude Code shell gotcha:** each Bash call is a separate shell, so `export` doesn't
persist. Pass `--profile <P>` on every command, or chain `export X=… && databricks …`.

```bash
databricks auth profiles                       # list (never auto-pick — let the user choose)
databricks jobs list --profile dev-aws
```

> Harness sessions: tokens MUST be U2M via a `claude_code*` profile (never M2M/`DEFAULT`)
> — see `rules/harness/databricks-profile.md`. Full auth detail: `references/auth-profiles.md`.
>
> **Exception — sandboxes with no `databricks` CLI binary.** The U2M rule presumes the CLI is
> installed, because `auth_type=databricks-cli` shells out to it. In a Claude Science sandbox
> the binary is often absent (not on `PATH`, not at `~/.local/bin/databricks`), so every
> `claude_code*` profile fails there and an OAuth **M2M** service-principal profile is the
> working path. Prefer U2M wherever the CLI exists; fall back to M2M only when it does not,
> and say so.

## SDK (essentials)

```python
from databricks.sdk import WorkspaceClient
w = WorkspaceClient()                          # or WorkspaceClient(profile="dev-aws")
w.jobs.list(); w.clusters.list()
w.files.upload("/Volumes/cat/schema/vol/f", contents, overwrite=True)
w.api_client.do(method="GET", path="/api/2.0/...")   # REST escape hatch
```

The SDK is synchronous — wrap in `asyncio.to_thread(...)` inside async code. More:
`references/sdk.md`.

## CLI (essentials)

Modern CLI **≥ 0.288.0** (`databricks --version`). Unity Catalog list/get use
**positional** args, not flags:

```bash
databricks tables list <CATALOG> <SCHEMA> --profile P     # NOT --catalog-name
databricks warehouses list --profile P                    # NOT sql-warehouses
# Fast data exploration (auto-detects a warehouse):
databricks experimental aitools tools discover-schema cat.schema.t --profile P
databricks experimental aitools tools query "SELECT * FROM cat.schema.t LIMIT 10" --profile P
```

Install + full quick-ref: `references/cli.md`.

## Jobs (essentials)

Multi-task DAGs via SDK, CLI, or DABs. Default to **serverless** for notebook/Python
tasks; use a `job_cluster_key` when you need a specific runtime/GPU/policy.

```yaml
resources:
  jobs:
    etl:
      name: "[${bundle.target}] ETL"
      schedule: {quartz_cron_expression: "0 0 2 * * ?", timezone_id: UTC}
      tasks:
        - {task_key: extract, notebook_task: {notebook_path: ../src/extract.py}}
        - {task_key: load, depends_on: [{task_key: extract}], run_if: ALL_SUCCESS,
           notebook_task: {notebook_path: ../src/load.py}}
```

Task types, triggers, params → `references/jobs.md`; reliability →
`references/jobs-monitoring.md`; full examples → `references/jobs-examples.md`.

## Asset Bundles (essentials)

```bash
databricks bundle validate -t dev      # always validate before deploy
databricks bundle deploy   -t dev
databricks bundle run my_job -t dev
```

One bundle, many environments via `targets` + `${var.x}` (override per target or with
`--var`). Structure, variables, promotion: `references/asset-bundles.md`. A real
multi-env bundle with per-env infra injection: `references/portal-databricks.md`.

## MCP tools

`mcp__databricks__*` tools cover SQL (`execute_sql`), Jobs, Unity Catalog, volumes,
serving, vector search, and workspace switching. Volumes:
`manage_volume_files` (`action=list|upload|download|delete`, `volume_path=/Volumes/…`).
Workspace switch: `manage_workspace` (`action=status|list|switch|login`, session-scoped).
UC detail: `references/unity-catalog.md`.

## Scripts (`scripts/`)

Runnable helpers that bootstrap auth via the harness `DatabricksClient()` (private
`CLAUDE_CODE_DBX_*`, `claude_code` U2M profile) and resolve config via the repo `DBX_*`
convention — never mix the two namespaces (see `references/env-decouple.md`).

| Script | Does |
|--------|------|
| `scripts/install_cli.sh` | Install the pinned Databricks Go CLI to `~/.local/bin` (Linux/WSL, standalone) |
| `scripts/setup_oauth.sh` | U2M OAuth login (`--host`); enforces `claude_code*` profile, rejects DEFAULT/M2M |
| `scripts/check_auth.py` | `token_health()` + whoami + resolved host (redacted) — auth sanity check |
| `scripts/query.py` | Run SQL against a warehouse (auto-pick or `--warehouse-id`); `--catalog` default `digitalbtd-dev` |
| `scripts/describe_table.py` | `catalog.schema.table` → schema + row count + stats |
| `scripts/list_resources.py` | List jobs / warehouses / clusters / serving-endpoints (`--kind`) |

Setup scripts (`.sh`) are standalone; Python helpers run from a repo with the harness
`sdk/` importable. Onboarding walkthrough: `references/setup.md`. Full usage:
`scripts/README.md`. Fetching SP (M2M) creds stays in repo tooling (`make fetch-sp-creds`
/ `scripts/ci-sp-env.sh`) — not a global script (needs `oc` + repo secret scopes).

## DigitalBTD repos (dev access)

Working on `/home/edotau/digitalbtd-insilico-suite` (or the portal repos)? The config is the
**repo-root** `.databrickscfg`, not `~/.databrickscfg`, and the dev catalog name contains a
hyphen. Both are handled by this skill's `kernel.py`:

```python
w = dbx_client()                                     # sp-dev, repo-root config, dev workspace
dbx_query(w, f"SELECT count(*) FROM {dbx_table(table='mmseqs_results')}")
```

Full setup — venv registration, all three workspaces, profile status, allowlist behaviour and
the read-only exploration rule: `references/digitalbtd-dev.md`. Read it before the first
`WorkspaceClient` call in a DigitalBTD repo.

## Guardrails

Never without explicit user confirmation: delete a serving endpoint or running cluster;
rotate/invalidate an active PAT; commit `~/.databrickscfg` or any token/secret; print a
full token (redact to last 4). CI/CD authenticates as a service principal, never
personal OAuth/PAT.

## Token refresh hooks

`SessionStart` checks freshness and refreshes if < 5 min remain; `PostToolUseFailure`
auto-recovers on 401/403 and retries once. The credential ladder is 24h PAT → M2M → 60-min
U2M JWT, all SDK-resident — inspect via `python -m sdk.token_store --check|--remaining` or
`python -m sdk.token_display`. Detail in `references/harness-auth.md`,
`references/auth-profiles.md`, and `rules/harness/databricks-profile.md`.
