# Data Reconciliation & Rollback

How to prove a migration is correct before cutover, and how to reverse it if not. Every phase
needs a tested rollback *before* you run it — a migration without a rollback is a gamble.

## Reconciliation — prove source and target agree

Layer these cheapest-first; each catches drift the previous misses:

1. **Row counts** — compare source vs target per table. Account for soft-deletes and filters
   (count with the same `WHERE`). Fast, catches gross failures (a stalled backfill).
2. **Checksums / hashing** — hash a stable-ordered subset of critical columns on both sides and
   compare. Sample for huge tables (e.g. every Nth id, or a date window). Catches value drift
   that counts miss.
3. **Business-logic aggregates** — run the critical queries (sums, revenue totals, active-user
   counts) on both systems and diff the results. Catches transform bugs that per-row checks pass.

**Delta detection** (the reconciliation workhorse):

```sql
SELECT 'missing_in_target' AS issue, s.id FROM source s
WHERE NOT EXISTS (SELECT 1 FROM target t WHERE t.id = s.id)
UNION ALL
SELECT 'extra_in_target', t.id FROM target t
WHERE NOT EXISTS (SELECT 1 FROM source s WHERE s.id = t.id);
```

**Principles:** reconciliation runs must be **idempotent** (safe to re-run), **non-destructive**
(report first, repair as a separate reviewed step), and **audited** (log every correction). On
Databricks, Delta time-travel gives you a free point-in-time source-of-truth to diff against.

## Rollback by layer

| Layer | Rollback mechanism | Anchor to prepare first |
|-------|--------------------|-------------------------|
| **Django schema** | run the migration's reverse (`migrate <app> <prev>`) | every migration reversible; test `migrate` down in staging |
| **Data** | restore from snapshot / Delta time-travel to a pre-migration version | snapshot + recorded version/timestamp at each checkpoint |
| **Service (OpenShift)** | flip Route back to blue / `oc rollout undo` | blue pods kept warm; prior ImageStream tag retained |
| **Infra (Terraform)** | re-apply prior tfvars/state | prior state + tfvars in version control; `prevent_destroy` on stateful |

## Rollback triggers — decide these BEFORE cutover

Define the objective thresholds that auto-abort, so the call isn't made under pressure:

- error rate > baseline + X% for N minutes
- p99 latency > threshold
- reconciliation drift > tolerance (non-zero breaking count)
- a critical business metric drops > Y%

If a trigger fires, execute the phase's rollback — don't debug forward in production.

## Runbook skeleton (fill per migration — this replaces a generic plan generator)

**Pre:** plan approved · rollback tested in staging · monitoring + triggers configured ·
snapshot/version anchors captured · `schema_compat.py` shows no un-handled BREAKING · owners + comms plan set.

**During:** execute phases in order · watch KPIs + reconciliation at each gate · log deviations ·
if a trigger fires → roll back the phase, don't push forward.

**Post:** all success criteria met · reconciliation clean · monitor 72h · update docs/runbook ·
decommission legacy only after the soak · retro.

Keep the runbook in the repo/PR, not in a generated JSON blob — it's a living checklist owned by
the migrating team.
