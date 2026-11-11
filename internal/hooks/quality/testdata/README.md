# testdata — golden fixtures (frozen from the retired Python generator)

## skeleton/ (claude-code workflow skeleton)
Pattern output was frozen from `workflow_skeleton.py` before its retirement; that script is no longer a live oracle.
No file input — CLI args only. No cwd sensitivity (no path embedded in output):

    claude-code workflow skeleton pipeline --name review-changes
    claude-code workflow skeleton parallel --name fan-out-audit
    claude-code workflow skeleton evaluator --name gen-verify
    claude-code workflow skeleton orchestrator --name plan-then-build
    claude-code workflow skeleton pipeline              (default-name.want)
    claude-code workflow skeleton bogus                  (exit 2, usage error)
