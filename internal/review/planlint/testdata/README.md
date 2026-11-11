# testdata — golden fixtures (live oracle: python3, this repo's checkout)

## assumptions/ (assumption_linter.py)
plan-standalone-harness.md <- docs/plans/plan-standalone-harness.md
simplicity-skill.md <- skills/simplicity/SKILL.md
Goldens captured with cwd = testdata/assumptions (the "source" field in the output is the
bare relative filename, so tests must `t.Chdir` here to match):

    cd testdata/assumptions && python3 <repo>/skills/evaluate-code/scripts/assumption_linter.py plan-standalone-harness.md [--json]
    cd testdata/assumptions && python3 <repo>/skills/evaluate-code/scripts/assumption_linter.py simplicity-skill.md [--json]
    echo "<stdin-case.txt content>" | python3 <repo>/skills/evaluate-code/scripts/assumption_linter.py - [--json]
    python3 <repo>/skills/evaluate-code/scripts/assumption_linter.py nosuch.md   (exit 0, "[error] ... not found")

## goals/ (goal_verifier.py)
plan-standalone-harness.md <- docs/plans/plan-standalone-harness.md (same commands/rules as above).
noplan.txt / stdin-case.txt are hand-written to exercise NO_PLAN and STRONG/WEAK verdicts.
