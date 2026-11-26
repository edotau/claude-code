"""Unity Catalog: https://databricks-sdk-py.readthedocs.io/en/latest/workspace/catalog/index.html"""

from databricks.sdk import WorkspaceClient
from databricks.sdk.service.catalog import VolumeType

w = WorkspaceClient(profile="dev")

# Browse catalog -> schema -> table, and read a table's columns.
for cat in w.catalogs.list():
    print(cat.name)
for sch in w.schemas.list(catalog_name="main"):
    print(sch.full_name)
for t in w.tables.list_summaries(catalog_name="main", schema_name_pattern="default"):
    print(t.full_name, t.table_type)
table = w.tables.get(full_name="main.default.trips")
print([(c.name, c.type_name) for c in table.columns])

# Create a schema and a managed volume, then upload a file into the volume.
w.schemas.create(name="scratch", catalog_name="main")
w.volumes.create(
    catalog_name="main",
    schema_name="scratch",
    name="files",
    volume_type=VolumeType.MANAGED,
)
with open("data.csv", "rb") as f:
    w.files.upload("/Volumes/main/scratch/files/data.csv", f, overwrite=True)
