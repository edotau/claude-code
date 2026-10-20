# Proposed Memory Updates

## Shared default

After finishing, if you learned something reusable, append a **Proposed Memory Updates** section. Do NOT write memory files yourself, and never touch the approval-gated RULES region. Frame each as topic-file content plus its one-line `MEMORY.md` TOC entry, with a rationale. The parent consolidates and writes the detail to a topic file, keeping `MEMORY.md` a lean index.

## Codex

After completing a review, verification, optimization, or validation, if you discovered something
reusable, append a **Proposed Memory Updates** section to your final report. Do NOT write memory files yourself, and never touch the approval-gated RULES region —
the parent orchestrator decides what gets persisted.

Candidate content: frequently missed constraints, common task patterns worth flagging,
project-specific evaluation criteria, baseline scores worth anchoring, effective
optimizations (what moved the needle), config patterns that worked, build/test quirks
(flaky tests, build order, platform differences), common skill-quality issues,
triggering-description improvements, recurring manifest failures.

Frame each as topic-file content plus its one-line `MEMORY.md` TOC entry, with a rationale. The
parent evaluates, consolidates, and (if accepted) writes the detail to a topic file, keeping
`MEMORY.md` a lean index.

## Opencode

After finishing, if you learned something reusable (a staleness signal unique to this harness, a
naming convention that worked, a reliable documentation source, an effective search query),
append a **Proposed Memory Updates** section. Do NOT write memory files yourself, and never touch the approval-gated RULES region —
the parent decides what gets persisted, keeping `MEMORY.md` a lean index.

## Copilot

At the end of either mode, propose (do not write) memory entries for durable facts only: a
fixture whose contract is easy to misread, a suite whose failure output is misleading, a repo
whose merge order is non-obvious, a gate that only fails post-merge. Skip anything the diff or
git history already records. Do NOT write memory files yourself, and
never touch the approval-gated RULES region — the parent decides what gets persisted.
