# Databricks Python SDK

Use the SDK (`pip install databricks-sdk`) when code, not a person, drives the workspace: a script, a job, a test
fixture. For a one-off look, the CLI (`cli.md`) is quicker. Model calls from agents still go through the harness
proxy, not `serving_endpoints.query`.

- Client: `WorkspaceClient(profile="dev")` reads `~/.databrickscfg`; with no arguments it uses `DATABRICKS_HOST` and
  `DATABRICKS_TOKEN`, `DATABRICKS_CONFIG_PROFILE`, or a notebook's own credentials. Never put a token in code.
- Long operations return a waiter: `create_and_wait`, `run_now_and_wait`, or `.result()` on the returned object.
- Lists are lazy iterators that page for you: loop over them, do not index them.

| Need | Example |
| --- | --- |
| Sign-in patterns | [examples/1-authentication.py](examples/1-authentication.py) |
| Clusters and jobs | [examples/2-clusters-and-jobs.py](examples/2-clusters-and-jobs.py) |
| SQL warehouses and statements | [examples/3-sql-and-warehouses.py](examples/3-sql-and-warehouses.py) |
| Unity Catalog (catalogs, schemas, tables, volumes) | [examples/4-unity-catalog.py](examples/4-unity-catalog.py) |
| Model serving and vector search | [examples/5-serving-and-vector-search.py](examples/5-serving-and-vector-search.py) |

Look up anything else rather than guessing a signature:

- SDK reference: <https://databricks-sdk-py.readthedocs.io/en/latest/>
- REST API: <https://docs.databricks.com/api/workspace/introduction>
- All Databricks docs as an index an agent can fetch: <https://docs.databricks.com/llms.txt>
