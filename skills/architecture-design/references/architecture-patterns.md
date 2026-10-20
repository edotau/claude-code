# Architecture Patterns

Patterns a typical small-to-medium platform uses or should reach for, with trade-offs: a web
app as a **modular monolith**, data/ML work as a **staged pipeline**. Distributed-systems
patterns (microservices mesh, event sourcing, CQRS with a broker) are listed at the end so you
can recognize and resist them when a design over-reaches.

## Index
1. [Modular monolith](#1-modular-monolith) — the web app
2. [Layered architecture](#2-layered-architecture) — inside each module
3. [Pipeline / staged DAG](#3-pipeline--staged-dag) — data and ML work
4. [Hexagonal (ports & adapters)](#4-hexagonal-ports--adapters) — isolating external IO
5. [Job-driven async](#5-job-driven-async) — batch + scheduled work
6. [Patterns to resist](#6-patterns-to-resist)

---

## 1. Modular monolith

One deployable project, internally partitioned into modules (Django apps, Rails engines, Go
packages) with clear boundaries.

**Use when:** medium team, single deploy unit acceptable, boundaries clarifying over time.

| Pros | Cons |
|------|------|
| One deploy, one database, simple transactions | Scaling is all-or-nothing |
| Module boundaries enable later extraction | Drifts back to coupled if discipline lapses |
| Team ownership per module | |

**Key rule:** modules communicate through their public interface (services / schemas), not
by importing each other's internals. A `from other_app.internal import …` is a boundary
violation → **should-fix**.

---

## 2. Layered architecture

**Within each module.** Presentation → business → data, one direction only.

```
app/
├── views / handlers      # presentation — HTTP in/out, no business logic
├── services              # business logic — orchestration, rules
├── schemas / serializers # boundary validation
├── models / repository   # data access
└── tasks                 # background jobs — async business logic
```

**Layer violations** (a view doing ORM aggregation, a model issuing HTTP calls) are the
common smell — `project_architect.py` flags mixed concerns and god files. Move business
logic out of handlers into services.

---

## 3. Pipeline / staged DAG

Sequential stages, each consuming the prior stage's output (parse → transform → enrich →
summarize). Stages are pure-ish transforms over tables or files.

**Use when:** data flows through ordered transformations; stages have distinct compute
profiles; you want to re-run from a checkpoint.

| Pros | Cons |
|------|------|
| Each stage independently testable + restartable | Intermediate materialization cost |
| Stages map cleanly to scheduler/orchestrator tasks | Ordering coupling between stages |
| Easy to parallelize a fan-out | Schema drift between stages must be guarded |

**Design rules:** a stage reads named inputs and writes named outputs; no stage reaches
around its predecessor; error-message substrings asserted by tests are part of the contract —
preserve them.

---

## 4. Hexagonal (ports & adapters)

**Where to isolate external platforms, model endpoints and IO.** Core logic depends on an
interface (port); the vendor/HTTP/file specifics live in an adapter.

**Use when:** business logic shouldn't know whether it's talking to a live service or a local
fixture — which is exactly what makes it testable offline.

```
core/scoring.py              # pure, no SDK import
ports/prediction_port.py     # interface the core depends on
adapters/http_endpoint.py    # real adapter → remote service
adapters/local_fixture.py    # test adapter → canned responses
```

**Payoff:** tests patch the adapter, not the world. If a function is hard to test because it
reaches straight into a vendor SDK client, the missing seam is a port — a design smell, not a
testing problem.

---

## 5. Job-driven async

Asynchronous/scheduled work without a message broker:

- **A scheduler / orchestrator** (cron, Airflow, a data platform's jobs) — pipeline batches,
  training, scheduled refreshes.
- **A task queue the web framework already uses** (Celery, Sidekiq, RQ) — app-side background
  tasks (email, reports, webhooks).

**Use when:** work is batchable or scheduled. **Don't** model it as an event stream unless
the platform already runs one — see §6.

---

## 6. Patterns to resist

Real patterns, but adopting one is a significant decision that needs an ADR and an owner.
Flag them when a design reaches for one without that justification:

| Pattern | Why resist | What to do instead |
|---------|-----------|--------------------|
| **Microservices** | Splitting adds network, deploy and ops surface no one may own | Modular monolith with clean boundaries (§1) |
| **Event sourcing / CQRS** | Needs an event store and broker; heavy complexity at modest scale | Plain CRUD; table history/audit columns cover most audit needs |
| **New message broker** | An unowned operational surface if nothing runs one today | Scheduler or existing task queue (§5) |
| **New datastore** | Another system to back up, secure and upgrade | `JSONB`-style column or the existing analytical store |
| **API Gateway / BFF layer** | One UI + one backend rarely needs it | Backend routing + auth at the edge |

The test: *does this pattern solve a problem the platform actually has today?* If it's
solving for hypothetical future scale, it's speculative complexity (discipline principle #2)
→ recommend the simpler stack-native option.
