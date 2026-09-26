# testdata — golden fixtures (live oracle: python3, this repo's checkout)

## quality/ (code_quality_checker.py)
Inputs are real files copied from this repo (paths.go <- internal/paths/paths.go,
gate-loop.js <- workflows/gate-loop.js). Goldens captured with cwd = testdata/quality:

    python3 <repo>/skills/evaluate-code/scripts/code_quality_checker.py paths.go [--json]
    python3 <repo>/skills/evaluate-code/scripts/code_quality_checker.py gate-loop.js [--json]
    python3 <repo>/skills/evaluate-code/scripts/code_quality_checker.py . [--json]        (dir.want*)
    python3 <repo>/skills/evaluate-code/scripts/code_quality_checker.py nonexistent.go    (notfound.want, exit 1)

The tool embeds `Path(args.path).resolve()` in its output — tests must `t.Chdir` into this
exact testdata dir so the absolute path in the golden matches what RunQuality computes.

## complexity/ (complexity_checker.py)
gate-loop.js <- workflows/gate-loop.js (JS: regex heuristic, exact parity expected).
assumption_linter.py <- skills/evaluate-code/scripts/assumption_linter.py (Python: AST-based in
the original; the Go port only ESTIMATES Python complexity, so only shape/structure is asserted,
not exact scores/findings).

    cd testdata/complexity && python3 <repo>/skills/simplicity/scripts/complexity_checker.py gate-loop.js [--json]
    cd testdata/complexity && python3 <repo>/skills/simplicity/scripts/complexity_checker.py assumption_linter.py [--json]
    python3 <repo>/skills/simplicity/scripts/complexity_checker.py nosuchfile.py --json   (exit 0, status:error)
