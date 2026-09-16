# Performance Review Lens

Find and fix the *real* bottleneck — the one the data points at, not the one you guessed.
This is the performance procedure a coding agent runs — inline during a review, or as a proactive
pass on a slow stage / SLA breach (profile first, then apply the fix).

## The one rule: measure before you optimize

**Never optimize on a hunch.** Profile/measure first, fix the top offender, measure again. An
"optimization" you can't show moving a number is just churn — and often the wrong spot (the slow
line is rarely the one that looks slow). Order: reproduce the slowness → measure → fix the biggest
contributor → re-measure → stop when it meets the SLA. Don't micro-optimize a path that isn't hot.

```bash
python -m cProfile -s cumtime script.py | head -30   # where does wall-time actually go?
# Web apps: a per-request SQL + timing profiler (e.g. django-silk, the framework debug toolbar)
# Go: go test -bench . -cpuprofile cpu.out && go tool pprof cpu.out
# Spark/distributed: the stages/SQL tab of the engine UI — skew, shuffle, spill are the usual culprits
```

## Highest-value target: N+1 queries

The single most common real bottleneck in ORM code (Django shown). A loop that touches a related object fires
one query per row — 1 + N instead of 1 or 2.

```python
# N+1 — one query for tasks, then one MORE per task for .project (and per task for .assignee)
for task in Task.objects.all():
    print(task.project.name, task.assignee.email)

# Fixed — select_related (FK/1:1, SQL JOIN) + prefetch_related (M2M/reverse FK, 2nd query)
for task in Task.objects.select_related("project", "assignee").prefetch_related("labels"):
    print(task.project.name, task.assignee.email)
```

Detect it: count queries in a test (`assertNumQueries`) or watch django-silk. On **Spark**
the analogue is a per-row Python UDF or a lookup inside a loop — vectorize / broadcast-join instead.

## Detectors (run these, then judge)

| Symptom | Look for | Fix |
|---------|----------|-----|
| Slow list/detail endpoint | N+1 queries (silk/`assertNumQueries`) | `select_related` / `prefetch_related`; `.only()`/`.values()` for wide rows |
| Query > 1s | `SELECT *`, missing index, full scan (`EXPLAIN`) | select needed columns, add index, paginate |
| O(n²) hot path | nested loops on same data, repeated `in list` searches, sort-in-loop | `set`/`dict` for O(1) lookup; sort once outside the loop |
| Exponential recursion | recompute of same subproblem | memoize (`functools.lru_cache`) |
| Memory grows unbounded | unbounded cache/list, listeners/timers without cleanup, closures holding big refs | bound the cache (LRU/TTL), explicit cleanup, drop references |
| Spark stage slow | skew, wide shuffle, spill, per-row UDF | repartition/broadcast, avoid UDFs, cache reused DataFrames |
| Many serial I/O calls | independent awaits run one-by-one | `asyncio.gather` / batch; cache with TTL |

## Algorithmic quick-ref

| Pattern | Cost | Better |
|---------|------|--------|
| Nested loop over same data | O(n²) | `set`/`dict` → O(1) lookup |
| Repeated `x in list` | O(n)/search | membership `set` |
| Sort inside loop | O(n² log n) | sort once outside |
| String `+=` in loop | O(n²) | `"".join(parts)` |
| Recompute in recursion | O(2ⁿ) | memoize |

## Report format

State the win, not just the finding: **file:line · impact (measured ms/queries/MB) · fix · expected
delta**. "N+1 in `tasks/views.py:42` — 380 queries/request → 2 with `select_related`; ~300ms→40ms."
Rank by measured impact; a finding without a number is a hypothesis, label it as one.

## Common mistakes

- **Optimizing without measuring** — you'll fix the wrong line. Profile first, every time.
- **Micro-optimizing a cold path** — spending effort where it can't move the SLA.
- **`select_related` for M2M/reverse-FK** — that's `prefetch_related`; `select_related` is FK/1:1 only.
- **Adding an index without checking the query plan** — confirm `EXPLAIN` actually uses it; a wrong
  index is write-cost with no read win.
- **Caching to hide an N+1** — fix the query; a cache over a bad access pattern just moves the bug.
- **Trading readability for an unmeasured micro-win** — don't. Simplicity first unless the profile demands it.

## Memory-leak patterns (long-running jobs / services)

When memory grows unbounded (an OOM after hours), look for:
- Event listeners / signal handlers registered without cleanup
- Timers / scheduled callbacks never cancelled
- Closures holding large references alive past their use
- Caches that grow without an eviction bound (LRU/TTL)
- Detached DOM nodes (frontend) still referenced by JS

Fix: bound the cache, explicit teardown of listeners/timers, drop references, weak refs where apt.

## Frontend / bundle (when the surface is a web app)

- **Bundle > 500KB gzip** → code-split, lazy-load routes, tree-shake dead exports.
- **Network** → parallelize independent requests (`Promise.all`/`asyncio.gather`), cache with TTL,
  debounce rapid-fire calls, stream large responses, enable gzip/brotli, paginate large sets.

## Audit report template (for a full proactive pass)

When running a *proactive* audit (a slow stage / SLA breach, not an inline review), report as:

```markdown
# Performance Audit Report
## Executive Summary — score, critical-issue count, recommendation count
## Critical Issues
### 1. [title]  ·  file:line  ·  Impact: causes ~XXXms delay  ·  Fix: [description]
## Recommendations — priority-ordered
## Estimated Impact — [metric] improvement: XX%
```

## Red flags — act immediately

| Issue | Action |
|-------|--------|
| Bundle > 500KB gzip | code-split, lazy-load, tree-shake |
| Query > 1s | add index, optimize query, cache result |
| Memory growing unbounded | check for the leak patterns above |
| CPU spikes | profile, find the hot path |
| N+1 queries | batch or JOIN (`select_related`/`prefetch_related`) |

## Related

- this file is the **performance-optimizer reference** (absorbs the retired `performance-optimizer`
  agent) — any coding agent runs it: inline during review, or proactively on a slow stage / SLA breach
