# Rule — DigitalBTD Databricks access

**Applies when** a task touches Databricks from a DigitalBTD repo
(`digitalbtd-insilico-suite`, `digitalbtd-portal-databricks`, `digitalbtd-portal-backend`,
`digitalbtd-rapid`): any `WorkspaceClient`, warehouse query, Unity Catalog read, job submit,
bundle deploy, or `.databrickscfg` question.

## Do this first, before the first client call

1. **Point at the repo config.** `DATABRICKS_CONFIG_FILE=<repo>/.databrickscfg`.
   `~/.databrickscfg` does not exist in these repos; without the variable a valid profile fails
   with `default auth: cannot configure default credentials`, which reads as a credential
   problem but is a path problem. `dbx_client()` in `kernel.py` does this.
2. **Use `sp-dev`** unless the user names another profile. `claude_code*`/`DEFAULT` are U2M and
   need the `databricks` CLI binary, which the sandbox usually lacks; `sp-qa` and `sp-prod` are
   currently rejected with `invalid_client`.
3. **Check the host is allowlisted** before diagnosing any auth failure. A non-allowlisted
   workspace burns ~5 minutes on a metadata timeout and then reports a generic auth error. The
   allowlist is per-hostname with no wildcards — dev, qa and prod are three separate grants.
4. **Backtick the catalog.** `digitalbtd-{dev,qa,prod}` contain a hyphen; unquoted they parse as
   subtraction. Use `dbx_table(...)`.
5. **Use the repo venv** (`environment='insilico-dev'`), never a fresh one — it carries
   Nexus-only packages that cannot be reinstalled from public PyPI.

## Default posture

Read-only exploration: proceed. Anything that **writes** a table, starts a rebuild / recluster /
index / provisioning job, submits to **qa or prod**, or mutates a cluster or warehouse: **ask
first.** Prefer `--dry_run` / `validate`, and note that `mmseqs_pipeline.py run --backend
databricks --dry-run` does not honour the flag and submits for real.

Never print values from `.databrickscfg`, `.env` or `provider.env`.

Full detail: `references/digitalbtd-dev.md`.
