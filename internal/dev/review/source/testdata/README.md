# testdata — golden fixtures (frozen captures of the retired Python tools; no regeneration command)

## quality/ (code_quality_checker.py)
Inputs are real files copied from this repo (paths.go <- internal/paths/paths.go,
gate-loop.js <- workflows/gate-loop.js). Goldens were captured with cwd = testdata/quality;
`dir.want*` is the whole directory, `notfound.want` a missing path (exit 1).

The tool embeds `Path(args.path).resolve()` in its output — tests must `t.Chdir` into this
exact testdata dir so the absolute path in the golden matches what RunQuality computes.

## complexity/ (complexity_checker.py)
gate-loop.js <- workflows/gate-loop.js (JS: regex heuristic, exact parity expected).
assumption_linter.py is a copy of the retired linter (Python: AST-based in the original; the Go
port only ESTIMATES Python complexity, so only shape/structure is asserted, not exact
scores/findings).
