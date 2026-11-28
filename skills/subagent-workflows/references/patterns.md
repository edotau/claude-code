# Agent Design Patterns (this harness)

Every row maps to code this repo runs. Serves `../SKILL.md` Phase 0: pick the shape here and fill each node with a leaf
type.

## Execution mode: subagents and plan waves

Agent teams (`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`) are not used; the harness keeps leaves terminal. Plan
ownership: `../SKILL.md` → Who holds the plan.

Concurrency: `../SKILL.md` → Concurrency.

## Patterns we run

The sections below describe task shapes. Dispatch uses `../SKILL.md` → Handoff contract.

### Pipeline

```text
scope: find -> merge findings -> rederive blind -> fix -> repair affected tests -> cheap checks
```

### Fan-out / fan-in

- Manual form: `Agent` calls in one message with `Owns:` files; fan-in reads every diff and the seam between leaves
  (`../SKILL.md` Phase 3).

### Expert pool

```text
task -> size -> repair | audit | evaluate | plan
```

- Tiers follow `rules/workflows/master-workflow.md` Phase 0.
- Specialists by model alias: Haiku sizes, gates and records; Sonnet finds, fixes and tests; `opus`/`fable` for
  planning, security, architecture and judgment calls.

### Producer-reviewer

```text
fix -> test repair -> cheap checks
finder claim -> blind re-derivation from file:line + symptom -> reproduced | dropped | unverified
```

- A re-deriver never sees the finder's reasoning. Guard test: `rules/standards/testing-guidelines.md`.
- Owner mid-turn messages reaching workflow agents is an open defect.

### Supervisor

```text
main -> explore -> plan -> parallel leaves -> collector -> sign-off
```

- Ownership: `../SKILL.md` → Handoff contract. Run-state: `claude-code agents tree`; proof an external agent ran:
  `ask: ran on` line (carried asks via `claude-code agents ask --carry`).

### Sequential plan and tournament

- Sequential plan: dispatch each dependent task when its input is ready. Final review and validation:
  `rules/standards/testing-guidelines.md` → Several worktrees: collapse, then test once.
- Tournament: baseline the evaluation, send the same brief to each candidate, then re-measure the results.
  A tie is not a win. Preserve losing work for the owner's decision; integrate the winner through the collector.
  Tree selection and cleanup: `rules/standards/worktree-git.md` → Worktrees.

### Not used

Agent teams / `TeamCreate` (experimental, one per session, no nesting); loop-until-dry and the modal sweep (retired or
pre-pass only); peer-to-peer agent debate (refute/rederive is one-way and blind); saved JS workflow scripts (removed).

## Split vs merge; skill vs agent vs brief

`rules/harness/agent.md` → Authoring (when to split or merge a role), Agent and command files
(skeleton, frontmatter) and Prose instructs, hooks guarantee (CLI registry, workflow receipt gates).

## Lessons from the trial

- Stream discovery into fixes; final review belongs to collector sign-off.
- Record in-run, not in a hook: PostToolUse(Workflow) fires at launch.
- Re-derive blind, and treat a failed re-derivation as unverified, never refuted.
- A fixer that is not `done` is red even when the gate passes.
- Route findings to the package that owns the file.
- Final sign-off: `rules/standards/testing-guidelines.md` → Several worktrees: collapse, then test once.
- Revert a leaf's out-of-scope hunk; a leaf never "fixes" files outside its list.
- Pin `model` and `effort` on every stage with one verbatim `stepOf`.
- Fix every finding, LOW included, in the round; only owner-judgment items go to a report.
