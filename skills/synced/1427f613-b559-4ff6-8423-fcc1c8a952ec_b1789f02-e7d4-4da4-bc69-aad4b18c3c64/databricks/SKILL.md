---
name: databricks
description: Databricks auth, Python SDK (WorkspaceClient, Volumes, Clusters, Jobs), CLI, REST API, and databrickscfg profiles.
---

# Databricks — Auth, SDK, Config & Docs

## When to Load

Any task involving: Databricks workspaces, clusters, SDK calls (WorkspaceClient),
volume file operations, token management, profile switching, or Databricks CLI.

---

## Auth Priority

The Databricks SDK resolves credentials in this order (earlier wins):

1. Explicit token passed to SDK/client constructor
2. M2M OAuth env vars: `DATABRICKS_CLIENT_ID` + `DATABRICKS_CLIENT_SECRET`
3. `DATABRICKS_TOKEN` env var
4. `~/.databrickscfg` profile — controlled by `DATABRICKS_CONFIG_PROFILE`

**Project rule**: canonical profile is `claude_code` (never `DEFAULT`).

## Token Lifecycle

| Type | Lifetime | Refresh |
|------|----------|---------|
| PAT | Long-lived | Manual rotation |
| M2M OAuth JWT | ~60 min | Refresh 5 min before expiry |
| Azure AD | Managed by SDK | Automatic |

## ~/.databrickscfg Structure

```ini
[claude_code]
host  = https://dbc-2f9779f1-045b.cloud.databricks.com
token = ...

[dev]
host          = https://dbc-2f9779f1-045b.cloud.databricks.com
client_id     = ...
client_secret = ...
```

---

## Workspace Config Switching (MCP)

Use `mcp__databricks__manage_workspace` tool:

| Intent | Action |
|--------|--------|
| Current workspace? | `action="status"` |
| List available | `action="list"` |
| Switch profile | `action="switch", profile="<name>"` |
| Login (OAuth) | `action="login", host="<url>"` |

Switch is session-scoped — resets on MCP server restart.

---

## Python SDK

**Docs**: https://databricks-sdk-py.readthedocs.io/en/latest/

### Setup

```python
from databricks.sdk import WorkspaceClient

w = WorkspaceClient()                    # Auto-detect from env/config
w = WorkspaceClient(profile="claude_code")  # Named profile
```

Key surfaces (full signatures in the SDK docs linked below):

- **Files / Volumes**: `w.files.upload(file_path, contents)`, `w.files.download(file_path)` (context manager), `w.files.list_directory_contents(dir)`; `upload_from`/`download_to` with `use_parallel=True` for large files.
- **Clusters**: `w.clusters.list()`, `w.clusters.create_and_wait(...)`, `w.clusters.start(id).result()`, `w.clusters.delete(id)`.
- **Jobs**: `w.jobs.create(...)`, `w.jobs.run_now_and_wait(job_id=...)`. For job DAGs, task types, triggers, DABs → see the **`databricks-jobs`** skill.
- **Direct REST**: `w.api_client.do(method="GET", path="/api/2.0/...", body={...})` for endpoints without an SDK wrapper.
- **Errors**: `from databricks.sdk.errors import NotFound, PermissionDenied` — catch these around `get`/`delete`.
- **Async (FastAPI)**: the SDK is synchronous — wrap calls in `asyncio.to_thread(lambda: list(w.clusters.list()))`.

---

## Databricks CLI

```bash
databricks --version          # >= 0.278.0
databricks --profile claude_code clusters list
databricks auth token --profile claude_code
```

---

## Documentation Reference

**Full docs index**: `https://docs.databricks.com/llms.txt`

### SDK URL Pattern

```
https://databricks-sdk-py.readthedocs.io/en/latest/workspace/{category}/{service}.html
```

| Category | Services |
|----------|----------|
| `compute` | clusters, cluster_policies, instance_pools |
| `catalog` | catalogs, schemas, tables, volumes, functions |
| `jobs` | jobs |
| `sql` | warehouses, statement_execution |
| `serving` | serving_endpoints |
| `files` | files, dbfs |
| `workspace` | repos, secrets, workspace |

---

## Hooks (Token Refresh)

| Hook | Purpose |
|------|---------|
| SessionStart | Check JWT expiry, refresh if < 5 min remaining |
| PostToolUseFailure | Auto-recover on 401/403, retry once |

---

## Guardrails

Never without explicit user confirmation:
- Delete serving endpoint or running cluster
- Rotate/invalidate active PAT
- Commit `~/.databrickscfg` or tokens to git
- Print full token to stdout (redact to last 4 chars)

---

## Unity Catalog

### Volumes (MCP)

| Tool | Usage |
|------|-------|
| `mcp__databricks__manage_volume_files` | `action="list/upload/download/delete"`, `volume_path="/Volumes/cat/schema/vol/path"` |
| `mcp__databricks__get_volume_folder_details` | Schema, row counts, stats for a volume path |

### System Tables

```sql
-- Lineage
SELECT source_table_full_name FROM system.access.table_lineage
WHERE target_table_full_name = 'cat.schema.table' AND event_date >= current_date() - 7;

-- Audit (permission changes)
SELECT event_time, user_identity.email, action_name FROM system.access.audit
WHERE action_name LIKE '%GRANT%' OR action_name LIKE '%REVOKE%'
ORDER BY event_time DESC LIMIT 100;

-- Billing (DBU by workspace)
SELECT workspace_id, sku_name, SUM(usage_quantity) AS dbus
FROM system.billing.usage WHERE usage_date >= current_date() - 30
GROUP BY workspace_id, sku_name;
```

Reference files: `../databricks-unity-catalog/5-system-tables.md`, `6-volumes.md`, `7-data-profiling.md`
