# Remote compute on a Databricks cluster

Run a CPU workload (a benchmark, a profile, a timing gate) on a quiet single-node cluster when the local box is too
loaded to trust a timing. This is driver-only shell work over SSH, not a Spark job and not model traffic.
`<p>` is the CLI profile, `<id>` the cluster id, `<alias>` the SSH host name you choose. Keep real ids, workspace
hosts and aliases out of tracked files: they belong in memory or the vault.

## 1. Warm the cluster (minutes, so start it first)

```bash
databricks clusters get <id> --profile <p> -o json | rtk jq '{state,node_type_id,autotermination_minutes}'
databricks clusters start <id> --profile <p> --no-wait    # TERMINATED -> PENDING -> RUNNING
```

- Starting costs money until the cluster's own `autotermination_minutes` stops it: say so, and start it only on the
  owner's ask. Never create, edit, resize or restart a cluster for this; never terminate one you did not start.
- An auth error means the profile's login has expired: the owner logs in (the repo's `make dbx-login` where one
  exists); do not run a login from a session.

## 2. Connect: the CLI's SSH tunnel, no key on the cluster

```bash
databricks ssh setup --name <alias> --cluster <id> --profile <p>   # once per machine; writes an ssh Host entry
rtk ssh -o BatchMode=yes <alias> 'hostname; python3 --version; nproc; uptime; df -h /local_disk0 | tail -1'
```

- Needs a Databricks CLI that has the `ssh` command (`databricks ssh --help`) and a dedicated (single-user) cluster.
- `setup` edits only the local ssh config. Nothing is added to the cluster, no inbound port is opened, and the
  cluster is not restarted. Do not put a public key in the cluster's `ssh_public_keys`: that edits and restarts it.
- The tunnel logs in as `root` on the driver, starts the cluster on connect by default, and its SSH server stops
  10 minutes after the last connection (`--shutdown-delay`). The first connect can take a minute or more.
- Every command prints tunnel status lines first (`Starting`, `Job`, `Health`, `Uploading`, `Received`,
  `Checking`): filter them before parsing output.
- No tunnel available: the command-execution API works too (`databricks api post /api/1.2/contexts/create`, then
  `/api/1.2/commands/execute` with Python and `subprocess`); destroy the context when done.

## 3. Ship: trees, not history; local disk, not a volume

```bash
rtk ssh <alias> 'mkdir -p /local_disk0/<job>/head /local_disk0/<job>/base'
git archive <head-sha> | ssh <alias> 'tar -x -C /local_disk0/<job>/head'
git archive <base-sha> | ssh <alias> 'tar -x -C /local_disk0/<job>/base'
rsync -a -e ssh <asset-dir>/ <alias>:/local_disk0/<job>/head/<asset-dir>/    # only what the code loads
```

- `git archive` sends one commit's tree with no history, so nothing old or unrelated leaves the machine.
- Before copying any data, list it and check it against the project's data rules (held-out sets, private
  sequences, secrets). Scan the tree too: a fixture can hold a copy of restricted data.
- Work on `/local_disk0`. A Unity Catalog volume or DBFS is network storage: it adds noise to a timing and is
  shared with other people's jobs. The driver's disk is lost when the cluster stops.
- Symlinked asset directories do not travel in `git archive`: copy the real files.

## 4. Environment

```bash
rtk ssh <alias> 'cd /local_disk0/<job> && python3 -m venv venv && venv/bin/pip install -q -e head'
```

- Check `python3 --version` against the project's requirement first, and whether the driver reaches the package
  index (`curl -s -o /dev/null -w "%{http_code}" https://pypi.org/simple/pip/`). With no route, build wheels locally
  and copy them.
- If `venv` fails for lack of `ensurepip`, create it with `--without-pip` and bootstrap pip inside it.
- Pin numerical libraries (numpy) to the local versions, or a head-versus-base ratio is the only number that
  compares across hosts. Record both hosts' versions in the report.

## 5. Run, fetch, clean up

```bash
rtk ssh <alias> 'cd /local_disk0/<job> && nohup venv/bin/python <driver> > run.log 2>&1 & echo started'
rtk ssh <alias> 'uptime; tail -5 /local_disk0/<job>/run.log'
rsync -a -e ssh <alias>:/local_disk0/<job>/results/ <local-results-dir>/
rtk ssh <alias> 'rm -rf /local_disk0/<job>/data'    # shipped data sets go; code and venv may stay for a rerun
```

- Start long runs with `nohup` (or `tmux`) and log to a file: a dropped SSH session must not kill the run.
- Record `uptime` with every timing. Judge speed as head against base on the same host, interleaved, medians.
- A run with no open connection and no Spark activity may look idle to the cluster's autotermination: for a run
  longer than a few minutes keep a session open or check on it, and copy results back before you finish.
- Leave the cluster running for its own autotermination unless the owner asks to stop it
  (`databricks clusters delete <id> --profile <p>` terminates; it does not remove the cluster).

## Dispatching it

Hand the whole job to one leaf with the cluster id, alias, the two shas, the data exclusion list and the result
directory; the main session keeps any gate verdict. One leaf at a time per `/local_disk0/<job>` directory.
