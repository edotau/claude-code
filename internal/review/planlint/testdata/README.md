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

## skeleton/ (workflow_skeleton.py)
No file input — CLI args only. No cwd sensitivity (no path embedded in output):

    python3 <repo>/skills/subagent-workflows/scripts/workflow_skeleton.py pipeline --name review-changes
    python3 <repo>/skills/subagent-workflows/scripts/workflow_skeleton.py parallel --name fan-out-audit
    python3 <repo>/skills/subagent-workflows/scripts/workflow_skeleton.py evaluator --name gen-verify
    python3 <repo>/skills/subagent-workflows/scripts/workflow_skeleton.py orchestrator --name plan-then-build
    python3 <repo>/skills/subagent-workflows/scripts/workflow_skeleton.py pipeline              (default-name.want)
    python3 <repo>/skills/subagent-workflows/scripts/workflow_skeleton.py bogus                  (exit 2, argparse error)
