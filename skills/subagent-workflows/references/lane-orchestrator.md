# Lane Orchestrator

Many leaf-sized tasks can be grouped into lanes of disjoint files. The main session owns the lane checklists,
dispatch, fan-in and commits; a lane is a scheduling group, not another manager subagent.
Dispatch syntax and relay behavior: `../SKILL.md` → Leaf types. Keep leaves terminal.

## Lane plan

Keep `lanes/<lane>.md` beside the implementation plan (`rules/harness/docs-placement.md`). Each checklist line names
one verifiable outcome, owned absolute paths, the selected leaf type and its dependencies. Split tasks before they
need a nested orchestration layer. One writer per file; a later wave owns overlapping work.

## Dispatch and verify

- The manager sends ready, disjoint leaves together, with a complete brief per `prompts.md`; each owns one checklist
  line. New tests wait for the manager's live check; repairs of existing tests may run beside implementation.
- Fan-in per `../SKILL.md` → Fan-in; return gaps to the same leaf with changed evidence. Apply
  `rules/standards/quality.md` to the combined diff; no leaf runs the full suite or commits.
- The manager lands named files only when authorized, then records the resulting hash on completed checklist lines.
  An edit-only task remains uncommitted and records checked results without a hash.

Explicitly requested batch scripts use `claude-code agents ask --batch` per its help. They do not bypass ownership,
permission or per-account concurrency limits, and are not a hidden fallback for unavailable Agent-tool dispatch.
