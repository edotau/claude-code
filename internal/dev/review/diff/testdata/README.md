# testdata — golden fixtures (frozen captures of the retired Python tools; no regeneration command)

## diff/ (diff_surgeon.py)
synthetic-noise.diff is hand-written to cover every noise category (whitespace, comment-only,
docstring-addition, quote-style-swap) in one small hunk. real-commit-range.diff is a saved
`git diff c6965de..0977f77` from this repo's own history (hermetic — never reads live git state).

RunDiff always returns 0 (its own doc comment); this diverges from the Python script's actual
exit codes (1 on a missing --file via an uncaught traceback, 2 on a bad flag via argparse).

## pr/ (pr_analyzer.py)
build_repo.sh builds a throwaway git repo with FIXED author/committer name/email/date, so the
commit hashes (and thus every field in the output) are byte-identical on every rebuild — verified
by diffing two independent builds' commit hashes before capture. The goldens are the analyzer's
`--base main --head feature` output on that repo.

## report/ (review_report_generator.py)
Uses `--pr-analysis`/`--quality-analysis` to feed hand-written pr-fixture.json / quality-fixture.json
(stand-ins for pr_analyzer.py / code_quality_checker.py output) — this keeps the report test
hermetic without needing a real git repo or the quality checker's exact output.

Timestamp lines ("Generated:"/"**Generated:**"/"generated_at") are normalized to a placeholder
before comparing (see report_test.go's normalizeReportTimestamp) — everything else is exact.

## gate/ (code-review-gate.sh)
No Python CLI existed for this one (a bash hook that shelled out to the other two scripts) —
gate_test.go builds a throwaway git repo in-process instead and asserts RunGate's documented
contract (always exit 0, prints the same section markers as the shell script). No golden capture.
