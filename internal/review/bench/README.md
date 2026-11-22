# Benchmark history and epochs

`review bench` appends schema-1 evidence to each package's `history.jsonl` beside its saved runs.
Each measured benchmark carries its source file and the SHA-256 of that file's exact bytes.
Sub-benchmarks share their parent function's file hash. History retains separate CPU-suffixed names.

Run headers store the function-to-file/hash map as a `# bench-epochs:` JSON comment. A file rewrite
starts a new epoch; evidence from an earlier epoch cannot establish stability or production relevance
in the new one. A source change during measurement fails the run and asks for a rerun.

Comparisons conservatively require identical package epoch maps. If the rolling baseline differs,
the current run becomes a new package baseline without a comparison. This also applies when moving
from an old baseline without hash metadata to a new hashed run. Other saved run files remain intact.
`--decide` returns 3 for incompatible epochs; it never treats them as an improvement or regression.
Two legacy files without metadata remain comparable for existing callers.

This package does not run new benchmark code against a parent commit's production code. The lane
driver must arrange those measurements with identical benchmark files before calling `--decide`.

Read-only verdict reports need neither a Git checkout nor benchstat:

```bash
cd /Users/edotau/.claude
claude-code review bench --verdicts --pkg internal/shell --format json
```

Normal benchmark reports also include verdicts. Verdict precedence is:

| Verdict | Evidence in the current epoch |
| --- | --- |
| dead | Explicitly recorded subject state is `gone` |
| off-path | Explicitly recorded production path is `off` |
| noisy | Latest run has load/noise flags, within-run CV >5%, or last five quiet medians have CV >5% |
| costly | Latest measured cost exceeds two seconds per sample |
| idle | No measurement, fewer than two samples, unverified production path, or no comparison/decision |
| idle | Epoch is at least 30 days old and has never been compared or has no decisive evidence in 30 days |
| working | Stable samples, recorded production relevance, and comparison or decisive evidence |

Cross-run CV uses matching machine, OS, architecture, Go version, CPU count, and GOMAXPROCS.
A quiet significant delta or a named kept/reverted scorecard row supplies decisive evidence.
Historical rows are ordered by timestamp; reports use the current time for the 30-day rule.
Unknown production relevance is not evidence of `off-path` or `dead`.

Record observed evidence with an existing scorecard operation. `--benchmark` is the exact name in
the run (including subcase and CPU suffix); `--epoch` is its lowercase 64-character hash:

```bash
cd /Users/edotau/.claude
claude-code review bench --record --pkg internal/shell --what 'profile and decision evidence' \
  --verdict kept --benchmark 'BenchmarkCommentsEachHash/comments-18' --epoch '<hash from run>' \
  --production-path on --subject present
```

Production path accepts `on|off`; subject accepts `present|gone`. These are caller observations,
not inferred from benchmark names or the absence of profile data. Omitting them keeps prior evidence
within the same epoch. Recording new-epoch evidence does not carry old-epoch observations forward.

A measured hash transition appends a `bench-change` scorecard row naming the benchmark and both
hashes. Explicit changes use `--record --verdict bench-change --old-epoch <old> --epoch <new>` with
the same required `--pkg`, `--what`, and `--benchmark` fields. Such rows do not close fix candidates.

History rejects corrupt, incomplete, or newer-schema rows. Existing scorecard rows stay readable.
All verdicts are reports only. No code here deletes benchmarks or history, including `dead` results;
D6 requires the owner to approve every deletion batch.
