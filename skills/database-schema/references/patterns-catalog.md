# Migration Patterns Catalog

The reversible patterns for changing a running system without downtime, mapped to the reference
stack (Django-DRF, Databricks/Unity Catalog, Terraform, OpenShift-ODS). Each is phased and
rollback-able per step — that is the whole point.

## Schema / data patterns

### Expand-Contract (the default for any breaking schema change)

Never rename/drop/narrow in one deploy — old and new code run simultaneously during a rolling
release, so both must work against the schema at every instant.

1. **Expand** — add the new column/table *nullable or with a default* (backward-compatible). Ship.
2. **Dual-write / backfill** — new code writes both old+new; backfill history in batches.
3. **Migrate reads** — switch readers to the new shape once backfill is verified. Ship.
4. **Contract** — drop the old column/table only after nothing reads it. Ship.

Run `scripts/schema_compat.py --before X --after Y` first: any **BREAKING** finding means you
need this dance, not a direct migration.

- **Django:** each step is its own migration. A `RenameField` or non-null-without-default in one
  migration breaks the rolling deploy — split into add-nullable → data migration (`RunPython`,
  reversible) → make-non-null → drop-old across separate deploys. Keep every migration reversible.
- **Databricks / Unity Catalog:** additive `ALTER TABLE … ADD COLUMN` is safe on Delta; column
  drop/rename needs the expand-contract cycle plus a view alias during transition. Version the
  table (Delta time-travel) as the rollback anchor.

### Parallel schema / dual-write

Run the new schema alongside the old, route with a feature flag, reconcile continuously (see
`reconciliation-and-rollback.md`), cut over when drift is zero. Use when the transform is complex
enough that in-place expand-contract is risky.

### Change Data Capture (CDC)

Stream source changes to the target for large datasets — zero-downtime because the source stays
live. On Databricks, Delta **Change Data Feed** (`readChangeFeed`) is the native CDC source; land
into the target, verify counts + checksums, then cut over.

## Service patterns

### Strangler Fig

Put a router/gateway in front, replace functionality incrementally, retire the legacy path as new
paths prove stable. On **OpenShift**, the Route/Ingress + a routing rule (or a small proxy)
fronts old vs new Service; shift path-by-path. Monitor error/latency per path throughout.

### Blue-Green (the OpenShift rollback anchor)

Keep the current version (blue) running while deploying the new (green); flip the Route to green;
flip back instantly on trouble. This is the **fastest rollback** available — the old pods are
still warm. Pairs with your OpenShift rollback runbook. Under ODS, this maps to the
DeploymentConfig/rollout + Route switch.

### Canary

Deploy to a small traffic slice, watch KPIs (latency, error rate, business metric), ramp as
confidence grows. Use when you can't afford a full green environment or want gradual real-user
validation.

## Infrastructure (Terraform)

Version every change; keep the prior state recoverable. `prevent_destroy` on stateful resources
(DBs, buckets) so a plan can't nuke them mid-migration. Migrate with `terraform state mv` for
refactors (no resource recreation), `import` to adopt existing resources. Test the rollback plan
(re-apply prior tfvars) in a non-prod workspace first. See Terraform's own docs for
state/backend mechanics.

## Choosing

| Situation | Pattern |
|-----------|---------|
| Any breaking column change | Expand-Contract |
| Large table, can't take downtime | CDC (Delta CDF) |
| Complex transform, want to compare | Parallel schema + dual-write |
| Replacing a service incrementally | Strangler Fig |
| Need instant rollback on deploy | Blue-Green |
| Gradual real-user validation | Canary |
