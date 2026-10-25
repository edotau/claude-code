---
name: database-schema
description: 'Use when designing a relational database schema (tables, relationships, normalization, indexes, multi-tenancy, soft deletes, audit trail, RLS, ERD) or evolving one safely (zero-downtime migration, backfill, breaking-change analysis, rollback, reconciliation, cutover). Triggers "design a schema", "ERD", "normalize", "index strategy", "RLS policy", "migrate the schema", "expand-contract", "backfill", "zero downtime", "data reconciliation". Django/Postgres, Databricks/UC, Terraform. Datastore choice->architecture-design.'
---

# Database Schema

Two halves of the same lifecycle: **design** a relational schema from requirements, and **evolve**
it safely once it's in production. Grounded in one reference stack — Django models over Postgres (portal),
Databricks/Unity Catalog Delta (pipeline). For the "which datastore / which pattern" *architectural*
decision, use `architecture-design`; this skill owns the concrete schema and how to change it
without downtime.

## Which half do you need?

| Task | Read |
|------|------|
| New schema from requirements: entities, relationships, normalization, indexes, multi-tenancy, soft-delete, audit, RLS, ERD | `references/schema-design.md` |
| Changing a live schema: is it breaking? expand-contract, backfill, CDC, rollback, reconciliation, cutover | this SKILL.md's migration workflow + `references/patterns-catalog.md` + `references/reconciliation-and-rollback.md` |

## Core principle (migration)

Old and new code run **simultaneously** during any rolling deploy. So every schema change must be
backward-compatible at every instant, or it breaks production mid-release. When it isn't naturally
compatible, make it so with a **phased, reversible** pattern (expand-contract, strangler fig,
blue-green) — never a big-bang rename/drop/narrow. (Design the schema right up front —
`references/schema-design.md` — and you'll need this dance less often.)

## When to use / not use

**Use for:** schema *design* (tables/relationships/indexes/RLS/ERD) and schema *evolution*
(migration, backfill/CDC, breaking-change check, rollback, reconciliation, cutover).

**Not for:** the architectural decision itself (datastore choice, monolith-vs-services) —
`architecture-design`. Stack mechanics live in their skills: `django` (models + migration files),
`databricks-expert` (UC/Delta), `devops-engineer` (Terraform state, OpenShift rollout/Route).

## The migration workflow

1. **Classify the change.** Run `scripts/schema_compat.py --before X --after Y`. Any **BREAKING**
   finding (dropped/narrowed column, NULL→NOT NULL, added NOT NULL without default) means you need
   expand-contract, not a direct migration. Exit 1 makes it a CI gate.
2. **Pick a pattern** from `references/patterns-catalog.md` by situation (breaking schema →
   expand-contract; large table → CDC; incremental service swap → strangler fig; instant-rollback
   deploy → blue-green).
3. **Design the rollback + reconciliation first** (`references/reconciliation-and-rollback.md`):
   capture snapshot/version anchors, define objective rollback triggers, choose the reconciliation
   layers. A phase without a tested rollback doesn't run.
4. **Execute phased**, validating at each gate; reconcile; cut over only when drift is zero; roll
   back the phase (don't debug forward) if a trigger fires.
5. **Soak + retro** — monitor 72h, decommission legacy only after, keep the runbook in the repo.

## Compatibility check — the one tool

```bash
python3 scripts/schema_compat.py --before schema_v1.json --after schema_v2.json
# exits 1 if any BREAKING change → needs expand-contract
python3 scripts/schema_compat.py --before v1.json --after v2.json --json  # for CI
```

Classifies every change: dropped table/column, type narrowing (bigint→int, text→varchar), length
truncation, nullability tightening, and added-NOT-NULL-without-default — the changes that break a
rolling deploy. Additive nullable columns and widenings pass as safe.

## Quick reference

| Need | Pattern | Rollback anchor |
|------|---------|-----------------|
| Breaking column change | Expand-Contract (nullable → backfill → migrate reads → drop) | reversible Django migration / Delta time-travel |
| Large table, no downtime | CDC (Delta Change Data Feed) | source stays live |
| Replace a service | Strangler Fig (route path-by-path) | keep legacy path until stable |
| Instant-rollback deploy | Blue-Green (flip the Route) | blue pods warm; `oc rollout undo` |
| Gradual real-user validation | Canary (ramp traffic slice) | shrink slice to 0 |
| Infra change | Terraform phased apply | prior state + `prevent_destroy` |

## Common mistakes

- **Rename/drop/narrow in one deploy** — breaks the rolling release (old code hits the new schema).
  Use expand-contract; run `scripts/schema_compat.py` to catch it first.
- **Non-null column without a default** — old writers and existing NULL rows fail. Add nullable,
  backfill, then tighten.
- **No tested rollback** — "we'll figure it out" is not a rollback. Test the reverse in staging.
- **Cutover before reconciliation is clean** — validate counts + checksums + business aggregates.
- **Debugging forward in prod when a trigger fires** — roll back the phase; investigate offline.
- **Migration plan/runbook in a generated blob** — keep it in the repo/PR as a living checklist.

## Additional resources

- `references/schema-design.md` — design a schema: entities→tables, normalization, cross-cutting concerns
  (multi-tenancy/soft-delete/audit/version), index strategy, Postgres RLS, Django example, Mermaid ERD
- `references/patterns-catalog.md` — expand-contract, strangler, CDC, blue-green, canary, per stack
- `references/reconciliation-and-rollback.md` — reconciliation layers, delta queries, rollback-by-layer, triggers,
  runbook skeleton
- `scripts/schema_compat.py` — breaking-change classifier (before/after schema JSON → report; exit 1 on breaking)
