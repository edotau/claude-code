# Orchestrate

One objective across several phases: `explore → plan → [refine] → implement → simplify → review → verify`.
Run it for `<task>` with optional flags `--scan` (explore only) or `--refine` (adds the opt-in refine stage). Each
stage is a `rules/workflow/master-workflow.md` phase; that file owns the rules, this file only sequences them.

Before you start: `git branch --show-current`, `git worktree list`, and `ls ~/.claude/plans/stages/*.md` (an
existing stage document means a run is in progress — resume it).

## `--scan`

The Codebase Intelligence Protocol only (`rules/workflow/agents.md`): 2–3 parallel `Explore`/`gemini` agents scoped
to the area, merged into a Codebase Context Brief. With a task, chain the brief into plan mode; without one, print it
and stop. Skip for one-line fixes or when a scan is already warm this session.

## Default run

1. Derive a kebab-case task id; create `~/.claude/plans/stages/YYYY-MM-DD-<task-id>.md`:

   ```markdown
   # Task: <task-id>
   > **Description**: <task> · **Created**: <date> · **Status**: in-progress · **Refine**: <true|false>

   ## Findings
   ## Plan
   ## Refined Briefs
   ## Implementation Notes
   ## Simplification
   ## Review
   ## Verification
   ```

   Omit `## Refined Briefs` without `--refine`.
2. Run the stages in order. Each agent's prompt: "Read `<stage doc>` and complete the `<section>` section", plus
   that stage's instructions. The stage document is the only context passed between stages.

   | Stage | Runner | Writes |
   |---|---|---|
   | explore (Phase 2) | `Explore` ×2–3 | `## Findings` |
   | plan (Phase 4) | `Plan` (`agents/Plan.md`) — **stop for user approval** | `## Plan` |
   | refine (Phase 5, `--refine` only) | `general-purpose` | `## Refined Briefs` |
   | implement (Phase 6) | `code-worker` per sub-task (parallel-safe ones in one message) ‖ `copilot` | `## Implementation Notes` |
   | simplify (Phase 7) | `Skill(simplify)` over the integrated diff | `## Simplification` |
   | review | `evaluate-code` ‖ `security-reviewer`; optionally `claude-code codex review --adversarial --base <rev>` | `## Review` (SHIP / FIX REQUIRED / BLOCK) |
   | verify (Phase 8) | main session: the repo's gates, then the gate chain in `skills/problem-solving/references/verify.md` | `## Verification` |

   Status per stage follows `../SKILL.md` → Handoff contract.
3. Set the document's status to `complete` and summarize: what changed, review findings, whether verification
   passed, and `refine: ran` or `refine: skipped`.

The user may skip or start at any stage ("skip explore", "start from implement").
