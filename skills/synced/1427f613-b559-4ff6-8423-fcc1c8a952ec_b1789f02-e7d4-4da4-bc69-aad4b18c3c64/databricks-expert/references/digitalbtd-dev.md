# DigitalBTD — Databricks dev access

Repo-specific setup for `/home/edotau/digitalbtd-insilico-suite` and the DigitalBTD portal
repos. The generic auth ladder, SDK and CLI usage are in `SKILL.md` and
`references/auth-profiles.md`; this file records only what differs here.

## 1. Environment — register the repo venv, never rebuild it

The suite's venv is registered as `insilico-dev` (path-venv, Python 3.12.9, editable so imports
resolve to `src/`). Pass `environment='insilico-dev'` to `python`/`bash`/`r` and
`insilico_suite`, `pathogen_mimicry`, `kernel` and `databricks-sdk` are importable.

It holds **Nexus-only** packages (`ig-align`) that cannot be reinstalled from public PyPI, so do
**not** recreate it, `--force-reinstall` into it, or `pip install` globally. `make install` is
the repo's only supported bootstrap. To re-register after a fresh clone:

```python
manage_environments(mode="register", name="insilico-dev",
                    source_path="/home/edotau/digitalbtd-insilico-suite",
                    venv_path="/home/edotau/digitalbtd-insilico-suite/.venv")
```

`.venv/bin/python` symlinks into `/home/edotau/.pyenv`, so that path must also be granted or the
venv is unusable from a sandbox. The installed dist version can lag `pyproject.toml` — check
both before blaming a wheel mismatch.

## 2. Login — the two-line setup

```python
import os
os.environ["DATABRICKS_CONFIG_FILE"] = "/home/edotau/digitalbtd-insilico-suite/.databrickscfg"
from databricks.sdk import WorkspaceClient
w = WorkspaceClient(profile="sp-dev")
w.current_user.me()          # confirm before doing real work
```

Or `dbx_client()` from this skill's `kernel.py`, which does exactly this.

**Why it is needed.** The config is the repo-root `.databrickscfg`; `~/.databrickscfg` does not
exist. The SDK's last credential rung reads `$DATABRICKS_CONFIG_FILE` and only then
`~/.databrickscfg`, so without the variable a perfectly valid profile fails with
`default auth: cannot configure default credentials` — a *path* failure wearing a *credential*
failure's error message. `dbx_client.load_databrickscfg()` follows the same rule, and the
Makefile's `DBX_CFG` / `DBX_CLEAN_ENV` targets set it the same way.

## 3. Profiles

| Profile | Type | Status in-sandbox |
|---|---|---|
| `sp-dev` | OAuth M2M (client_id + secret) | **Works** — the path to use |
| `sp-qa`, `sp-prod` | OAuth M2M | Rejected by workspace OIDC: `invalid_client` |
| `DEFAULT`, `claude_code*` | `auth_type=databricks-cli` (U2M) | Fail — no `databricks` binary in the sandbox |

`claude_code*` profiles shell out to the CLI, so they cannot work where the binary is absent
(not on `PATH`, not at `~/.local/bin/databricks`). Prefer U2M wherever the CLI exists; M2M is
the fallback, not the default.

The repo's designed recovery for stale SP credentials is `get_oc_m2m_credentials()` →
`OpenshiftClient.get_secret_dict()` against the `digitalbtd-dbx-sp` secret
(`DBX_CREDENTIALS_SECRET`), also the fallback rung in `DbxClusterMngr.build_strategies()`. `oc`
is installed at `/usr/local/bin/oc` but has no session by default, so that path needs an
`oc login` (and likely a `~/.kube` grant plus the cluster API host allowlisted).

## 4. Three workspaces

Defined in `ENVIRONMENTS` in `src/insilico_suite/configs.py`; each entry is a plain dict with
`host` and `catalog` keys (not an object — `v["host"]`, not `v.workspace_url`).

| env | host | catalog |
|---|---|---|
| dev | `dbc-2f9779f1-045b.cloud.databricks.com` | `digitalbtd-dev` |
| qa | `dbc-26e210e7-0e07.cloud.databricks.com` | `digitalbtd-qa` |
| prod | `dbc-e9246bf1-8517.cloud.databricks.com` | `digitalbtd-prod` |

The sandbox network allowlist is **per-hostname with no wildcards**, so granting one workspace
does nothing for the others — a common failure is granting only the host named in an error
message. A non-allowlisted host burns a ~5-minute `/.well-known/databricks-config` metadata
timeout and then surfaces as a generic auth failure. **Confirm the host is allowlisted before
diagnosing credentials,** and wrap probes in a timeout so a blocked host cannot hang a cell for
ten minutes.

## 5. Querying dev

- Warehouse `65c031735de15d01` ("digitalbtd", 2X-Small serverless) — auto-starts in ~8 s on the
  first query, so allow for it in `wait_timeout`.
- Schema `insilico`.
- **The catalog name contains a hyphen and must be backticked in SQL:**
  `` `digitalbtd-dev`.insilico.mmseqs_results ``. Unquoted it parses as subtraction.
  `dbx_table(table="...")` builds the ref correctly.

## 6. Rule — read-only by default

Exploration (`SELECT`, `SHOW TABLES`, `count(*)`, coverage checks) is safe and needs no
confirmation. Ask first, every time, before:

- writing, replacing or dropping any table, or running `dbt build`/`run` against a catalog;
- starting a database rebuild, recluster, index build or provisioning job;
- submitting any job to **qa or prod**, or any real Databricks/OpenShift submit from the repo;
- deleting or resizing a cluster or warehouse.

Per the repo's `AGENTS.md`, prefer `--dry_run` / `validate` before any real remote submit:
`python -m insilico_ig_cli mhcii --input_fst input.fa --dry_run`,
`python src/mmseqs_pipeline.py validate --job-dir <dir>`. Note that
`run --backend databricks --dry-run` in `mmseqs_pipeline.py` does **not** honour the flag — it
returns before the dry-run branch and submits a real job.

Never print or echo values from `.databrickscfg`, `.env` or `provider.env`; naming that a value
was read from them is fine.
