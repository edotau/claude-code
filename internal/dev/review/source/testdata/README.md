# testdata — golden fixtures (frozen captures of the retired Python tools; no regeneration command)

## complexity/ (golden for `review simplify --complexity`)
gate-loop.js <- workflows/gate-loop.js (JS: regex heuristic, exact parity expected).
assumption_linter.py is a copy of the retired linter (Python: AST-based in the original; the Go
port only ESTIMATES Python complexity, so only shape/structure is asserted, not exact
scores/findings).
