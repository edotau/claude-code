# Performance review lens

Use within a review or proactively on slow stages/SLA breaches.

## The one rule: measure before you optimize

Reproduce → measure → fix the biggest contributor → remeasure → stop at the SLA.
Never optimize cold paths or trade readability for an unmeasured win; no number means hypothesis.
Python: `python -m cProfile -s cumtime script.py`; Go: `rtk go test -bench . -cpuprofile cpu.out`,
then `rtk go tool pprof -top cpu.out`. Web: per-request SQL/timing profiler (django-silk/debug toolbar).
Spark: stages/SQL UI for skew, shuffle, spill.

## Highest-value target: N+1 queries

A related-object lookup per row costs 1+N queries. Django: `select_related` joins FK/1:1;
`prefetch_related` fetches M2M/reverse FK separately. Check with `assertNumQueries` or django-silk.
Spark: replace per-row UDFs/lookups with vectorization/broadcast joins. Fix the query before caching.

## Symptoms → fixes

| Symptom | Inspect → fix |
| --- | --- |
| Slow list/detail | N+1 → related/prefetch queries; `.only()`/`.values()` for wide rows |
| Query >1s | `SELECT *`, missing index/full scan (`EXPLAIN`) → needed columns, used index, pagination |
| Nested loops/repeated list membership | O(n²) → set/dict O(1) lookup |
| Sort in loop | O(n² log n) → sort once |
| String `+=` in loop | O(n²) → `"".join(parts)` |
| Recomputed recursion | O(2ⁿ) → memoize (`functools.lru_cache`) |
| Unbounded memory | Unbounded caches, listeners/timers, closures, detached DOM → LRU/TTL, teardown, release refs |
| Slow Spark stage | Skew/shuffle/spill/UDFs → repartition/broadcast, avoid UDFs, cache reused DataFrames |
| Serial independent I/O | Sequential awaits → `asyncio.gather`/batch; TTL cache |
| Bundle >500KB gzip | No splitting → split/lazy-load/tree-shake; debounce, stream, compress, paginate |

## Report format

**file:line · measured ms/queries/MB · fix · expected delta**. Rank by measured impact; label unmeasured findings
hypotheses. Proactive audits add score/critical count, critical issues, priorities and estimated improvement per metric.
