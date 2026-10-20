# Technology Decision Guide

Decision matrices for a typical platform: a relational database behind a web app, an
analytical store for pipeline data, a task queue or scheduler for async work. Prefer the
option the project already runs over the generically "best" one — a new datastore or queue is
a cost someone must carry. If `references/local/` exists, its stack defaults win.

## Index

- [1. Data store selection](#1-data-store-selection)
- [2. Caching strategy](#2-caching-strategy)
- [3. Async / job execution](#3-async--job-execution)
- [4. Authentication strategy](#4-authentication-strategy)
- [5. API style](#5-api-style)
- [6. Compute placement](#6-compute-placement)
- [Quick reference](#quick-reference)

---

## 1. Data store selection

| Need | Default | Notes |
|------|---------|-------|
| App transactional data (users, projects, state) | **Relational DB** (PostgreSQL) | ACID, framework-managed migrations |
| Pipeline / analytical tables, large scans, time-travel | **The existing analytical store** (lakehouse / warehouse) | query via its SQL engine |
| Vector / similarity search | `pgvector` or the data platform's vector index | don't stand up a separate vector DB first |
| Large files / artifacts | **Object storage** | path-addressed blobs, not a relational store |
| Ephemeral cache / session | In-process, or Redis if already provisioned | see §2 before adding Redis |

**Decision flow:**

```text
App state (users/projects)?      ──► relational DB
Pipeline/analytical data?        ──► analytical store
Embeddings / similarity search?  ──► pgvector or platform vector index
Large file/artifact?             ──► object storage
Otherwise (cache/session)        ──► §2
```

**Avoid** introducing a datastore nothing else runs (Mongo, Dynamo, Cassandra) — it adds an
unowned operational surface. If flexible schema is the pull, a `JSONB` column usually covers it.

---

## 2. Caching strategy

| Pattern | Use case | Invalidation |
|---------|----------|--------------|
| Cache-aside (lazy) | Expensive query or endpoint call reused across requests | On write / TTL |
| In-process memo (`functools.lru_cache`, dict) | Per-key results inside one run | Process lifetime |
| Framework cache | View-level caching | TTL |

**Before adding Redis:** ask whether an in-process cache or a materialized table solves it.

| Data type | Suggested TTL |
|-----------|---------------|
| Auth tokens | managed by the SDK's token cache, not a generic cache |
| Model-endpoint responses | 1–5 min |
| Slow-changing query results | 5–60 min |
| Feature flags / config | 1–5 min |

---

## 3. Async / job execution

| Requirement | Use | Notes |
|-------------|-----|-------|
| Scheduled / batch pipeline | **Scheduler / orchestrator** (cron, Airflow, platform jobs) | the canonical home for pipeline work |
| App background task | **The framework's task queue** (Celery, Sidekiq, RQ) | email, reports, webhook fan-out |
| Long-running ML inference | **Model endpoint** or a batch job | not a web-app task |
| Cross-repo orchestration | **CI/CD pipeline** | build-time, not app runtime |

**Avoid** adding Kafka/RabbitMQ/SQS when nothing runs a broker — event-style fan-out can
usually ride the scheduler or task queue; introducing a broker needs an ADR and an owner.

---

## 4. Authentication strategy

Keep the auth surfaces distinct — don't conflate them:

| Surface | Mechanism |
|---------|-----------|
| End users (UI → API) | **OIDC / OAuth2** via the identity provider; the API validates the token |
| Developer → platform | **Interactive OAuth (U2M)** via a named profile — never an implicit default that points at prod |
| Service → service | **Machine identity** (service principal / workload identity), scoped per environment |

Auth/crypto/token changes are a **must-review** surface → route to `security-reviewer`.

---

## 5. API style

| Requirement | Choose | Why |
|-------------|--------|-----|
| CRUD + resource endpoints | **REST** | simplest tooling; framework-native |
| Flexible client queries | REST with filtering params first | add GraphQL only if the team already runs it |
| Model inference | **Endpoint REST** (often OpenAI-compatible) | |
| Internal same-language calls | direct import / SDK call | gRPC only across languages or at scale |

**Versioning:** URL path (`/api/v1/`).

---

## 6. Compute placement

**Does this run on the data platform or the app platform?**

| Data platform (jobs / clusters) when | App platform (containers / PaaS) when |
|--------------------------------------|---------------------------------------|
| Heavy data/compute (distributed, large scans, GPU) | Long-lived web service or dashboard |
| Scheduled pipeline / batch job | Request/response API with low latency |
| Model training or serving | Stateful UI session |
| Needs the analytical store natively | Needs routes, config maps, container builds |

When a workload spans both (a dashboard over an analytical query), the UI lives on the app
platform and calls the data platform via its SQL engine or endpoint — don't move the data
plane into the pod.

---

## Quick reference

| Decision | Default | Reach further only when |
|----------|---------|--------------------------|
| App data | Relational DB | analytical → analytical store |
| Analytical data | Existing analytical store | — |
| Vectors | pgvector / platform index | scale demands a dedicated store (ADR) |
| Cache | in-process / materialized table | distributed need → Redis (ADR) |
| Async | Scheduler / task queue | genuine event streaming → broker (ADR) |
| Auth (users) | OIDC via the IdP | — |
| API | REST | — |
| Compute | §6 placement test | — |
