
// ── Fill in before running ──
//  • Replace *_SCHEMA with JSON Schemas (or drop the {schema:...} opt for free-text agents).
//  • Every agent() payload needs a self-contained handoff (objective + constraints + artifacts
//    + acceptance + verify command) — see skills/subagent-workflows/SKILL.md. Cold-start test:
//    a fresh agent with zero context can execute it.
//  • Pipeline is the default (no barrier). Use parallel() only when a step needs ALL prior results.
//  • 'router' isn't a Workflow shape — it's an inline branch on the input, then dispatch. No skeleton.
