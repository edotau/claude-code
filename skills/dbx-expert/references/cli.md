# Databricks CLI in this harness

The `databricks` CLI is for workspace work (jobs, clusters, tables, bundles). Model traffic never uses it: that goes
through the harness proxy (`SKILL.md`). Full command reference: <https://docs.databricks.com/aws/en/dev-tools/cli/>.

## Install and sign in

- Needs v1.0.0 or later: `databricks -v`. Install or upgrade with `brew install databricks/tap/databricks` (macOS) or
  `curl -fsSL https://raw.githubusercontent.com/databricks/setup-cli/main/install.sh | sh` (Linux, WSL).
- The workspaces are the profiles `dev`, `qa` and `prod` in `~/.databrickscfg`: `databricks auth profiles` lists them.
- Sign in through the harness, which keeps the credential in the vault: `claude-code databricks login --profile <p>`
  (remote box: `claude-code --host <box> databricks login …`, see the `network-browser` skill). Check with
  `databricks current-user me --profile <p>`.
- Never pick a profile for the person when the task does not name one: list them and ask.

## Every command names its profile

Each Bash call is a fresh shell, so an `export` in one call is gone in the next. Pass `--profile <p>` on every
command, or chain: `export DATABRICKS_CONFIG_PROFILE=dev && databricks jobs list`.

```bash
databricks clusters list --profile dev
databricks jobs list --profile dev
databricks jobs get --job-id <id> --profile dev
databricks warehouses list --profile dev
databricks serving-endpoints list --profile dev
databricks catalogs list --profile dev
databricks schemas list <catalog> --profile dev            # positional, not --catalog-name
databricks tables list <catalog> <schema> --profile dev    # positional
databricks tables get <catalog>.<schema>.<table> --profile dev
databricks bundle validate|deploy|run <job> -t <target>    # from the bundle's folder
```

When a flag fails, `databricks <group> <command> --help` shows whether it takes positional arguments.

## Look at data

```bash
databricks experimental aitools tools discover-schema <catalog>.<schema>.<table> --profile dev
databricks experimental aitools tools query "SELECT * FROM <catalog>.<schema>.<table> LIMIT 10" --profile dev
databricks experimental aitools tools get-default-warehouse --profile dev
```

- Use names exactly as given; backtick-quote a part with a hyphen or space: `` `my-catalog`.schema.table ``.
- There is no `databricks sql execute` or `execute-statement`: use `aitools tools query`.
- Production is read-only from a session: query it, never change it.
