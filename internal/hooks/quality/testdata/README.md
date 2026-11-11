# testdata — golden fixtures (live oracle: python3, this repo's checkout)

## skeleton/ (workflow_skeleton.py)
No file input — CLI args only. No cwd sensitivity (no path embedded in output):

    python3 <repo>/skills/subagent-workflows/scripts/workflow_skeleton.py pipeline --name review-changes
    python3 <repo>/skills/subagent-workflows/scripts/workflow_skeleton.py parallel --name fan-out-audit
    python3 <repo>/skills/subagent-workflows/scripts/workflow_skeleton.py evaluator --name gen-verify
    python3 <repo>/skills/subagent-workflows/scripts/workflow_skeleton.py orchestrator --name plan-then-build
    python3 <repo>/skills/subagent-workflows/scripts/workflow_skeleton.py pipeline              (default-name.want)
    python3 <repo>/skills/subagent-workflows/scripts/workflow_skeleton.py bogus                  (exit 2, argparse error)
