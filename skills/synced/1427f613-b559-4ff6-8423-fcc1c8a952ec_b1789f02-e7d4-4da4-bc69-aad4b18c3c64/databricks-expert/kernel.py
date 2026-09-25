"""Helpers for DigitalBTD Databricks work (see SKILL.md)."""
import os

INSILICO_REPO = "/home/edotau/digitalbtd-insilico-suite"
INSILICO_DEV_WAREHOUSE = "65c031735de15d01"
INSILICO_DEV_CATALOG = "digitalbtd-dev"
INSILICO_SCHEMA = "insilico"


def dbx_client(profile="sp-dev", config_file=None, repo=None):
    """WorkspaceClient with DATABRICKS_CONFIG_FILE pointed at the repo-root .databrickscfg.

    Without this the SDK looks only at ~/.databrickscfg and raises
    'default auth: cannot configure default credentials' even for a valid profile.
    """
    from databricks.sdk import WorkspaceClient
    if config_file is None:
        if repo is None:
            repo = INSILICO_REPO
        config_file = os.path.join(repo, ".databrickscfg")
    if not os.path.exists(config_file):
        raise FileNotFoundError(f"no databricks config at {config_file}")
    os.environ["DATABRICKS_CONFIG_FILE"] = config_file
    return WorkspaceClient(profile=profile)


def dbx_query(w, sql, warehouse_id=None, wait="50s"):
    """Run one read-only statement. Returns {'state','columns','rows'} or raises on failure.

    Serverless warehouses auto-start on the first query (~8 s), so allow for that in `wait`.
    """
    from databricks.sdk.service.sql import StatementState
    if warehouse_id is None:
        warehouse_id = INSILICO_DEV_WAREHOUSE
    r = w.statement_execution.execute_statement(
        warehouse_id=warehouse_id, statement=sql, wait_timeout=wait)
    state = r.status.state
    if state != StatementState.SUCCEEDED:
        raise RuntimeError(f"statement {state}: {getattr(r.status, 'error', None)}")
    cols = [c.name for c in r.manifest.schema.columns]
    rows = r.result.data_array if (r.result and r.result.data_array) else []
    return {"state": str(state), "columns": cols, "rows": rows}


def dbx_table(catalog=None, schema=None, table=""):
    """Backtick-quote a table ref. The dev catalog has a hyphen and must be quoted."""
    if catalog is None:
        catalog = INSILICO_DEV_CATALOG
    if schema is None:
        schema = INSILICO_SCHEMA
    return f"`{catalog}`.{schema}.{table}" if table else f"`{catalog}`.{schema}"
