# Composite Workflow Templates

Choose a shape here; execution, arguments and authoring live in [saved-workflows.md](saved-workflows.md).
Dispatch ownership: `../SKILL.md` → Handoff contract.

## A. code-engineer: size, route, stream, record

Use for a requested workflow when no narrower script fits. Size the task, select the script, pass complete findings,
then record measured results. EPIC returns a plan. The route and stage schema live in `workflows/code-engineer.js`.

## B. code-workers: find, rederive, fix, test repair, gate, simplify, review, fix-all

Use for named files or packages; `fix:false` is an audit. Merge findings by location or symbol, rederive blind,
and send each confirmed finding to the file's owner. An unverified finding is not refuted.
Current script defaults and collector compatibility: [saved-workflows.md](saved-workflows.md).

## C. MEDIUM feature: intel, implementer, code-workers harden

Use for a feature or refactor spanning a few files. Intel identifies paths and dependencies; the implementer builds
the requested behavior; repair addresses observed defects. A repair-only stage cannot substitute for implementation.
Test selection: `rules/standards/testing-guidelines.md` → What gets a test, and what never does.

## D. EPIC: plan draft, owner approval, rounds over package groups

Draft the dated plan from `docs/templates/plan.md`, obtain plan approval, then group work by owned files and
dependencies. Continue from the first unticked task; record a report per round.
Fix findings within the owner's priority; send only owner-judgment decisions back.

## E. Main-session supervisor loop around a workflow

Launch the requested script, await its result, then follow `../SKILL.md` → Fan-in.
Collection and landing: `rules/standards/worktree-git.md` → Worktrees.
Keep decisions and measured results in the report; a script's completion claim is not sign-off.

## F. code-analyzer partition: scan, plan, waves

Use when shared files or exported changes make task order matter. Dependency scans name files, exports, importers
and hotspots. The script groups disjoint work into waves and gives hotspot files one final owner.
`fix:false, partition:true` returns the tasks without editing. See `workflows/code-analyzer.js` for partition args.
Tree selection: `rules/standards/worktree-git.md` → Worktrees.

## New composite: template

```text
Name:     <workflow-name; file and meta.name match>
Use when: <task shape>
Pattern:  <pipeline | fan-out | expert pool | producer-reviewer | hierarchical>
Stages:   <role, model/effort, input, output schema, cheap check, recorded evidence>
Args:     <required inputs, optional scope and stage overrides>
Cost:     <agents per unit, measured tokens; slice for first run>
Failure:  <failure mode and recovery>
Resume:   <resumeFromRunId or rerun args; budget-ended work stays with main>
Returns:  <findings, fixes, residuals, measured files/lines, decisions, report path>
```

### Checklist: adding it as a saved workflow

Follow [saved-workflows.md](saved-workflows.md) → Authoring.
Validation: `rules/standards/testing-guidelines.md` → Several worktrees: collapse, then test once.
