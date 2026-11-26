"""SQL warehouses and statements: https://databricks-sdk-py.readthedocs.io/en/latest/workspace/sql/index.html"""

from databricks.sdk import WorkspaceClient
from databricks.sdk.service.sql import StatementParameterListItem, StatementState

w = WorkspaceClient(profile="dev")

# Warehouses: list, start one and wait until it is RUNNING.
for wh in w.warehouses.list():
    print(wh.id, wh.name, wh.state)
warehouse_id = next(iter(w.warehouses.list())).id
w.warehouses.start(id=warehouse_id).result()

# Run a parameterized statement synchronously (wait up to 30s) and read the rows.
resp = w.statement_execution.execute_statement(
    warehouse_id=warehouse_id,
    statement="SELECT * FROM main.default.trips WHERE fare > :min LIMIT 10",
    parameters=[StatementParameterListItem(name="min", value="20", type="DOUBLE")],
    wait_timeout="30s",
)
if resp.status.state == StatementState.SUCCEEDED:
    cols = [c.name for c in resp.manifest.schema.columns]
    rows = [dict(zip(cols, r)) for r in resp.result.data_array or []]
    print(rows)
