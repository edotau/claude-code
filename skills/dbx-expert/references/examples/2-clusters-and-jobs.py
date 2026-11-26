"""Clusters and jobs: https://databricks-sdk-py.readthedocs.io/en/latest/workspace/compute/clusters.html"""

from databricks.sdk import WorkspaceClient
from databricks.sdk.service import jobs

w = WorkspaceClient(profile="dev")

# Clusters: list, pick a runtime and node type, create and wait, start/stop.
for c in w.clusters.list():
    print(c.cluster_id, c.cluster_name, c.state)
cluster = w.clusters.create_and_wait(
    cluster_name="scratch",
    spark_version=w.clusters.select_spark_version(latest=True, long_term_support=True),
    node_type_id=w.clusters.select_node_type(local_disk=True, min_memory_gb=16),
    num_workers=1,
    autotermination_minutes=30,
)
w.clusters.ensure_cluster_is_running(cluster.cluster_id)
w.clusters.delete(
    cluster_id=cluster.cluster_id
)  # terminates; permanent_delete removes it

# Jobs: create a notebook task on an existing cluster, run it and wait, read its output.
job = w.jobs.create(
    name="nightly",
    tasks=[
        jobs.Task(
            task_key="main",
            existing_cluster_id=cluster.cluster_id,
            notebook_task=jobs.NotebookTask(
                notebook_path="/Workspace/Users/me/nightly"
            ),
        )
    ],
)
run = w.jobs.run_now_and_wait(job_id=job.job_id)
print(
    run.state.result_state,
    w.jobs.get_run_output(run_id=run.tasks[0].run_id).notebook_output,
)
