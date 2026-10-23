# Orchestrate

Launcher: `/workflow:orchestrate [--scan | --refine] <task>`.

Pipeline: `explore → plan → [refine] → implement → simplify → review → verify`. `refine` is
**opt-in** — only when `--refine` is passed.

## `--scan` mode

Runs only the explore stage: the Codebase Intelligence Protocol (`rules/workflow/agents.md` →
"Codebase Intelligence Protocol") — 2-3 parallel Explore/Gemini agents scoped to the area the task
touches, merged into a Codebase Context Brief. With a task description, chain the brief into plan
mode; blank, print the brief and stop. No stage handoff document, no further stages. Skip for
single-line/typo fixes or when a scan is already warm this session.

## Default run

1. Parse the arguments. `--refine` present → `INCLUDE_REFINE=true` (strip it); otherwise `false`.
   Derive a kebab-case task ID from the remaining description (e.g. "add user auth" →
   `add-user-auth`).
2. Create the stage handoff document `~/.claude/plans/stages/YYYY-MM-DD-{task-id}.md` (include
   `## Refined Briefs` only when `INCLUDE_REFINE=true`):

   ```markdown
   # Task: {task-id}
   > **Description**: {task description}
   > **Created**: {date}
   > **Status**: in-progress
   > **Refine enabled**: {INCLUDE_REFINE}

   ## Findings
   ## Plan
   ## Refined Briefs
   ## Implementation Notes
   ## Simplification
   ## Review
   ## Verification
   ```

3. Run the stages sequentially. For each stage, dispatch an `Agent` (the subagent type in the table
   below) whose prompt is: "Read `~/.claude/plans/stages/YYYY-MM-DD-{task-id}.md` and complete the
   {stage} section", plus the stage's own instructions. Writing stages (implement, simplify) scope
   each agent to disjoint files in the shared tree. Skip `refine` entirely when
   `INCLUDE_REFINE=false`.

4. Stages — each is a `rules/workflow/master-workflow.md` phase; that file owns the rules, this
   table only names the runner:

   | Stage | Agent | Notes |
   |-------|-------|-------|
   | explore (Phase 2) | `Explore` | Structure, conventions, dependencies → `## Findings` |
   | plan (Phase 4) | `Plan` | Task breakdown per `agents/Plan.md` → `## Plan`; **stop for user approval** |
   | refine (Phase 5, optional) | `general-purpose` | Self-contained brief per sub-task → `## Refined Briefs` |
   | implement (Phase 6) | `code-worker` per sub-task (parallel-safe ones in one message) **‖ `copilot`** | Execute plan steps |
   | simplify (Phase 7) | `general-purpose` loading the `simplify` skill | Behavior-preserving cleanup — spec → simplify → quality order per `subagent-driven-development.md` |
   | review (Phase 1) | `general-purpose` loading `evaluate-code` **‖** one loading `security-reviewer` | Findings + SHIP / FIX REQUIRED / BLOCK |
   | verify (Phase 8) | main session: `Workflow gate-loop {cwd, scope}` (gate auto-detected), then the `approval-gate` checklist | — |

   For an independent second opinion at review, also run
   `claude-code ask --agent <leg> "<review brief with the diff range>"` from Bash (`claude-code
   agents` lists the available legs).

5. Update the stage document status to `complete`.
6. Print a summary: what was done, issues found in review, whether verification passed, and
   whether the refine stage ran (`refinement: ran` or `refinement: skipped (not requested)`).

Status handling per stage follows `subagent-driven-development.md` → "Status handling"
(`DONE`/`DONE_WITH_CONCERNS`/`NEEDS_CONTEXT`/`BLOCKED`).

The user can skip stages ("skip explore", "start from implement"). The stage handoff document is
the single source of truth for context between stages.
