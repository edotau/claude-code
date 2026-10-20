# System Design Workflows

Step-by-step workflows for the design tasks that come up most. Each ends in an **ADR** (see
template at the bottom) when the decision is consequential.

## Index
1. [New pipeline stage](#1-new-pipeline-stage)
2. [New web feature (API + UI)](#2-new-web-feature)
3. [Compute placement](#3-compute-placement)
4. [Capacity / scalability check](#4-capacity--scalability-check)
5. [Migration planning](#5-migration-planning)
6. [Writing an ADR](#6-writing-an-adr)

---

## 1. New pipeline stage

1. **Define the contract.** Named inputs (tables/files) → named outputs. Write the output
   schema down first.
2. **Place compute.** Heavy/distributed/GPU → a scheduled job on the data platform; light
   glue → wherever the caller runs. See §3.
3. **Isolate IO.** Put the platform/endpoint call behind an adapter (hexagonal) so the
   transform is testable offline.
4. **Fan-out plan.** If the stage iterates over a cross product, decide the parallelism unit
   and whether results memoize per key.
5. **Guard the seams.** Assert the output schema; preserve any error-message substrings that
   downstream tests check.
6. **Verify.** Unit-test the transform with a fixture adapter; smoke-run on a small slice.

---

## 2. New web feature

1. **Model first.** Add/extend models; generate the migration with the framework's tool
   (never hand-edit generated migrations).
2. **Layer it.** Business logic in a service, not the handler. Validate at the boundary
   (serializer / schema).
3. **Expose the API.** Versioned route (`/api/v1/`); a permission check enforces auth.
4. **Async?** Long work → background task, not an inline request. Inference → a model endpoint.
5. **UI side.** Component calls the API with the user's token; types match the API schema.
6. **Verify.** API tests (auth + happy + edge); component tests for the UI.

---

## 3. Compute placement

Run the placement test from `tech-decision-guide.md` §6. Decision tree:

```
Heavy data / distributed / GPU / scheduled batch? ──► data platform job
Model training or serving?                        ──► data/ML platform (job or endpoint)
Long-lived web service / API / UI session?        ──► app platform (containers/PaaS)
Spans both (UI over an analytical query)?         ──► UI on the app platform; data plane stays put
```

Document the choice in an ADR when it's non-obvious or sets precedent.

---

## 4. Capacity / scalability check

Usually not web-scale QPS math — the real questions:

- **Data volume:** will a stage's scan grow super-linearly? Partition/cluster the table; avoid
  full scans in hot paths.
- **Fan-out width:** cross products explode — bound them, memoize per key, and log anything
  you cap so silent truncation doesn't read as "covered everything."
- **Endpoint throughput:** batch calls; respect rate limits.
- **Web app:** N+1 ORM queries are the usual bottleneck — eager-load relations; route real
  perf work through `evaluate-code`'s performance lens (`references/performance.md`).

---

## 5. Migration planning

1. **Inventory.** What references the thing? Use `dependency_analyzer.py` for module
   coupling and `Grep` for call sites.
2. **Sequence.** Expand → migrate → contract. Add the new path, move callers, remove the old
   path — never big-bang.
3. **Data.** Additive schema evolution where possible; reversible migrations; no data-lossy
   step without a backup.
4. **Rollback.** Every step reversible until the contract phase. State the rollback in the
   ADR.
5. **Verify per step.** Tests green between each phase, not just at the end.

---

## 6. Writing an ADR

Keep it short. One file per decision under the project's `docs/` (or `plan/`).

```markdown
# ADR-NNN: <title>
Date: YYYY-MM-DD   Status: proposed | accepted | superseded

## Context
What forces are at play — requirements, constraints, the stack reality.

## Decision
The choice, stated plainly.

## Alternatives considered
- Option A — why not
- Option B — why not

## Consequences
- Positive: …
- Negative / accepted trade-off: …
- Follow-ups: …
```

An ADR is warranted when the decision is hard to reverse, sets precedent, or reaches outside
the stack-native defaults (new datastore, broker, auth surface, cross-cutting pattern).
