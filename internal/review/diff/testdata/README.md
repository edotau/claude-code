# testdata — golden fixtures (live oracle: python3, this repo's checkout)

## diff/ (diff_surgeon.py)
synthetic-noise.diff is hand-written to cover every noise category (whitespace, comment-only,
docstring-addition, quote-style-swap) in one small hunk. real-commit-range.diff is a saved
`git diff c6965de..0977f77` from this repo's own history (hermetic — never reads live git state):

    python3 <repo>/skills/evaluate-code/scripts/diff_surgeon.py --file testdata/diff/synthetic-noise.diff [--json]
    python3 <repo>/skills/evaluate-code/scripts/diff_surgeon.py --file testdata/diff/real-commit-range.diff [--json]
    python3 <repo>/skills/evaluate-code/scripts/diff_surgeon.py --file testdata/diff/empty.diff [--json]

RunDiff always returns 0 (its own doc comment); this diverges from the Python script's actual
exit codes (1 on a missing --file via an uncaught traceback, 2 on a bad flag via argparse) —
see test-repair report for the "unresolved" note.

## pr/ (pr_analyzer.py)
build_repo.sh builds a throwaway git repo with FIXED author/committer name/email/date, so the
commit hashes (and thus every field in the output) are byte-identical on every rebuild — verified
by diffing two independent builds' commit hashes before capture. Goldens:

    bash testdata/pr/build_repo.sh /tmp/pr-fixture-1
    cd /tmp/pr-fixture-1 && python3 <repo>/skills/evaluate-code/scripts/pr_analyzer.py . --base main --head feature [--json]

## report/ (review_report_generator.py)
Uses `--pr-analysis`/`--quality-analysis` to feed hand-written pr-fixture.json / quality-fixture.json
(stand-ins for pr_analyzer.py / code_quality_checker.py output) — this keeps the report test
hermetic without needing a real git repo or the quality checker's exact output.

    cd testdata/report && python3 <repo>/skills/evaluate-code/scripts/review_report_generator.py repo \
        --pr-analysis pr-fixture.json --quality-analysis quality-fixture.json [--format markdown|--json]

Timestamp lines ("Generated:"/"**Generated:**"/"generated_at") are normalized to a placeholder
before comparing (see report_test.go's normalizeReportTimestamp) — everything else is exact.

## gate/ (code-review-gate.sh)
No Python CLI exists for this one (it's a bash hook that shells out to the other two scripts) —
gate_test.go builds a throwaway git repo in-process instead and asserts RunGate's documented
contract (always exit 0, prints the same section markers as the shell script). No golden capture
here; this directory is intentionally empty.
