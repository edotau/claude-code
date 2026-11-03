# Performance review lens

Find the *real* bottleneck — the one the data points at. Run inline during a review, or as a proactive pass on a
slow stage or SLA breach (this absorbs the retired `performance-optimizer` agent).

## The one rule: measure before you optimize

An "optimization" you can't show moving a number is churn, and the slow line is rarely the one that looks slow.
Order: reproduce the slowness → measure → fix the biggest contributor → re-measure → stop when it meets the SLA.
Never micro-optimize a cold path or trade readability for an unmeasured win.

```bash
python -m cProfile -s cumtime script.py | head -30   # where does wall-time actually go?
go test -bench . -cpuprofile cpu.out && go tool pprof cpu.out
# Web apps: a per-request SQL + timing profiler (django-silk, the debug toolbar)
# Spark: the engine UI's stages/SQL tab — skew, shuffle and spill are the usual culprits
```

## Highest-value target: N+1 queries

A loop that touches a related object fires one query per row (1 + N instead of 1 or 2). Django shown:

```python
for task in Task.objects.all():                # N+1: .project and .assignee each query per row
    print(task.project.name, task.assignee.email)

for task in Task.objects.select_related("project", "assignee").prefetch_related("labels"):  # fixed
    print(task.project.name, task.assignee.email)
```

`select_related` is FK/1:1 (JOIN); `prefetch_related` is M2M/reverse FK (second query). Detect with
`assertNumQueries` or django-silk. On Spark the analogue is a per-row Python UDF or a lookup in a loop: vectorize
or broadcast-join. Don't cache over an N+1; fix the query.

## Symptoms → fixes

| Symptom | Look for | Fix |
|---|---|---|
| Slow list/detail endpoint | N+1 queries | `select_related` / `prefetch_related`; `.only()`/`.values()` for wide rows |
| Query > 1s | `SELECT *`, missing index, full scan (`EXPLAIN`) | select needed columns, add an index the plan actually uses, paginate |
| Nested loop over same data / repeated `x in list` | O(n²) | `set`/`dict` for O(1) lookup |
| Sort inside a loop | O(n² log n) | sort once outside |
| String `+=` in a loop | O(n²) | `"".join(parts)` |
| Recomputed subproblem in recursion | O(2ⁿ) | memoize (`functools.lru_cache`) |
| Memory grows unbounded | caches without LRU/TTL, listeners/timers never cleaned up, closures holding big refs, detached DOM nodes | bound the cache, explicit teardown, drop references |
| Spark stage slow | skew, wide shuffle, spill, per-row UDF | repartition/broadcast, avoid UDFs, cache reused DataFrames |
| Many serial I/O calls | independent awaits one by one | `asyncio.gather` / batch; cache with TTL |
| Bundle > 500KB gzip | no splitting | code-split, lazy-load routes, tree-shake; debounce, stream, compress, paginate |

## Report format

**file:line · impact (measured ms/queries/MB) · fix · expected delta** — "N+1 in `tasks/views.py:42` — 380
queries/request → 2 with `select_related`; ~300ms→40ms." Rank by measured impact; a finding without a number is a
hypothesis, labeled as one. A proactive audit adds an executive summary (score, critical count), the critical
issues, priority-ordered recommendations and the estimated improvement per metric.
