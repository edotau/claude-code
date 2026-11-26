"""Databricks SDK authentication: https://databricks-sdk-py.readthedocs.io/en/latest/authentication.html"""

from databricks.sdk import AccountClient, WorkspaceClient

# Named profile from ~/.databrickscfg (dev, qa, prod here; `databricks auth login -p <p>` writes it).
w = WorkspaceClient(profile="dev")
print(f"Authenticated as: {w.current_user.me().user_name}")

# No arguments: DATABRICKS_HOST/TOKEN or DATABRICKS_CONFIG_PROFILE from the env, or a notebook's own credentials.
w = WorkspaceClient()

# Account-level operations (users, workspaces, billing) need the account console host and account id.
a = AccountClient(profile="account")
for workspace in a.workspaces.list():
    print(f"Workspace: {workspace.workspace_name}")
