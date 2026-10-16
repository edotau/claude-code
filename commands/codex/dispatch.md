---
description: "Dispatch a task to the codex agent runner"
argument-hint: "[--format json] <task>"
allowed-tools: Bash
---

# /codex:dispatch — Task Dispatch via Codex

Runs `claude-code ask --agent codex` to hand any self-contained leg of work — implementation
review, analysis, a plan cross-check, or mechanical batch work — to a different model family
than the current session. Use `--format json` for a machine-parseable envelope when chaining
into another tool.

## Execution

```bash
claude-code ask --agent codex $ARGUMENTS
```

## Related

- `/codex:adversarial-review` — the review-specific sibling, with a session-correlated handoff file
- `claude-code agents` — list every available runner and whether it's launchable
