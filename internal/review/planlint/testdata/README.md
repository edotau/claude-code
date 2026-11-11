# testdata — golden fixtures (frozen captures of the retired Python tools; no regeneration command)

## assumptions/ (assumption_linter.py)
plan-standalone-harness.md and simplicity-skill.md are copies of documents this repo no longer has.
Goldens were captured with cwd = testdata/assumptions (the "source" field in the output is the
bare relative filename, so tests must `t.Chdir` here to match); `stdin-case.*` is the stdin run.

## goals/ (goal_verifier.py)
plan-standalone-harness.md is the same copy as above (same rules).
noplan.txt / stdin-case.txt are hand-written to exercise NO_PLAN and STRONG/WEAK verdicts.
