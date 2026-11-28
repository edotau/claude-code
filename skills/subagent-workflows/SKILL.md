---
name: subagent-workflows
description: >-
  Run multi-agent work end to end: shape selection (fan-out, lanes, waves, tournament), briefs, dispatch, fan-in
  and hand-back. Use for "fan this out", "dispatch agents", "write a brief", "which agent", "run the plan",
  "fix failing tests".
---

# Subagent Workflows

## Execution mode: subagent

Dispatch, roster and labels: `CLAUDE.md` guideline 3; leaves are terminal, so the main session is the one
orchestration layer. Agent file = who (`rules/workflows/agents.md`), brief's `Method:` skill = how, this skill = when
and in what order, [patterns.md](references/patterns.md) = which shape.

## Who holds the plan

| Shape | The plan lives in | Use when |
| --- | --- | --- |
| Subagent / skill | Claude, turn by turn | the next step depends on what the last one found |
| Plan waves (`agents ask --waves`) | the plan file | the steps are known up front and run in waves |

## Leaf types

Shell `agents ask` belongs in a carried dispatch, workflow stage or requested script (`rules/workflows/agents.md`).

| Leaf | Role |
| --- | --- |
| `claude` | Code leaf: Pi first; Sonnet in-context on Pi refusal. |
| `Explore` · `Plan` | Scoped scan (Codebase Context Brief) · Design in `docs/templates/plan.md` form. |
| `general-purpose` · `web-fetch` | Built-in work when no named role fits · Host page fetch (else `general-purpose`). |
| `codex`, `copilot`, `gemini`, `opencode` | External work through a Haiku relay running its prescribed ask. |
| `coworker` | Pi in code mode through a Haiku relay; raw tool output, rtk off. |
| `claude-code-guide` · `statusline-setup` | Host guidance on Claude Code/SDK/API · Host status-line configuration. |

Relays report the inner `ask: ran on` receipt and verdict; their Haiku wrapper is not the work model. Print a leaf's
description prefix with `claude-code agents label <type> [--model m] [--profile p] --task FILE`. Model by job: fix and
review-loop leaves run Sonnet (review stays another family, R6), mechanical fixes `Tier: fast` with overlay
`"model":"auto"` (R24), Opus/higher only for planning, security/guard design, architecture and judgment calls; `fable`
only for two large designs at once. Root pins and per-call flags select external execution; never overwrite another
machine's pins.

### Carried dispatch

For `Carry` agents (codex, copilot, gemini, opencode) or plain Pi, run the ask in the background and wait once
([agent-verbs.md](references/agent-verbs.md) → Carried dispatch details; policy:
[dispatch-policy-details.md](references/dispatch-policy-details.md)):

```bash
claude-code agents ask --carry --background --overlay '{"agent":"<agent>"}' [ask flags] - < BRIEF
claude-code agents result -f <id>
```

**Default leaf route:** a `claude` code leaf with no `Agent:` line tries Pi first and falls back in-context on Pi
refusal (reporting both on `Ran on:`); review, docs and gate leaves stay in-context. `Agent: pi` (or type `pi`)
retypes to `coworker` (`Codemode: off` keeps plain Pi). Briefs needing Agent/skills/MCP set `Agent: in-context`.

### External relay contract

Run the prescribed ask once ([agent-verbs.md](references/agent-verbs.md) → External relay checklist): reserve with
`agents brief --path <agent>`, launch via `agents ask --background` (never shell `&` or `run_in_background`), wait once
per turn with `agents result -f <id>` or `agents wait <id> --timeout 590s` (hand back after 4 waits if still running),
then call `SubagentHandback` with the work agent's `ask: ran on` receipt and verdict under `## Result`. Never silently
substitute an agent or manufacture a receipt. Hook gates: [hook-details.md](references/hook-details.md).

## Workflow

### Phase 0: intake and triage

Tier in `rules/workflows/master-workflow.md` → Phase 0 picks the shape; a one-line edit runs inline.

| Shape | Do |
| --- | --- |
| Single leaf · parallel fan-out | `Agent` call(s) with disjoint `Owns:` ([prompts.md](references/prompts.md)) |
| Which agent, model, workspace | [agents-and-models.md](references/agents-and-models.md) |
| Same question, many angles · fallback | Separate leaves; routes: [cost-routing.md](references/cost-routing.md) |
| Sequential plan · tournament | [patterns.md](references/patterns.md) |
| A plan's waves, observed | `agents ask --waves`; lifecycle: [agent-verbs.md](references/agent-verbs.md) |
| Many leaf-sized tasks · wave table | [lane-orchestrator.md](references/lane-orchestrator.md) |
| Fleet scan | Carried `copilot` ask with an explicitly requested fleet task; verify the returned diff |

### Phase 1: prepare — Handoff contract

Split until files would overlap or one leaf must wait on another's result. Full field rules, templates and report
format: [prompts.md](references/prompts.md) → Handoff contract details.

- **Brief (in):** cold-start self-contained (`Objective:`, `Agent:` before first blank line, `Task: Phase W.k`,
  `Tier: fast|standard|deep`, `Owns:`, `Method:`, `Context:`, `Constraints:`, `Verify:`, `Acceptance:`). Put `model` in
  `--overlay`, not `Model:`. Render with `claude-code agents brief --task F --own P…`; `agents diff --verify <id>`
  checks `Owned:`. Overlay keys: `rules/harness/agent.md` → Overlay.
- **Report (out):** ≤8-line verdict (`DONE` · `DONE_WITH_CONCERNS` · `NEEDS_CONTEXT` · `BLOCKED` or review verdict),
  `Ran on:` receipt (`self` only for in-context), per-item outcome, gate results, decisions needed, and report path
  `$(claude-code memory path reports YYYY-MM-DD-<name>.md)`. Scratch only under `/tmp/<job>/`, removed before hand-back.
- **Ownership:** one writer per file; leaves stay terminal; commit authority: `rules/standards/worktree-git.md` →
  Worktrees.

### Phase 2: dispatch — Concurrency, Dispatch workflow (R32) and Owner layer

- **Concurrency:** cap in `rules/harness/hooks.md`. Dispatch every unblocked task and refill on each hand-back,
  spreading leaves across Databricks profiles (R20) and vendor CLIs.
- **Dispatch workflow (R32):** tree selection/collection in `rules/standards/worktree-git.md` → Worktrees; validation
  in `rules/standards/testing-guidelines.md` → Several worktrees: collapse, then test once. Pi/external agents
  implement and fix; Claude verifies cheap checks; main integrates. Reviewer family differs from author's (R6); review
  loop: `rules/standards/quality.md` → Completion.
- **Owner layer (the contract):** `contract` on `agents ask --brief` defines `unit`, `kind`, `limits`,
  `role: owner|fleet` and `parent`. Wave loop, dispatch table and hot-file splitting:
  [lane-orchestrator.md](references/lane-orchestrator.md).
- **Tests:** `rules/standards/testing-guidelines.md`. Use implementer for small repairs or carried `copilot` ask with
  TEST and exact failing names; main owns the live check.

### Phase 3: integrate (Fan-in and Landing) & Phase 4: finish

- **Fan-in:** treat reports as claims — inspect diff, removed-code callers, ownership and producer/consumer seams.
  Simplify once over the merged diff before the shared other-family review.
- **Landing & finish:** follow `rules/standards/worktree-git.md` → Worktrees, `skills/using-git-worktrees/SKILL.md` →
  The lane protocol, and `rules/standards/quality.md`. Send gaps to a still-running owner; never relaunch a
  user-stopped leaf or resend a budget-ended task (main finishes partial work, R34). Plan ticks/strikes:
  `rules/standards/standing-requirements.md` → R1 and R2.

## Shared tree

Keep the main shell at the repo root and use absolute paths
(`skills/using-git-worktrees/references/shared-checkout.md`). Fix only owned files with `agents fix --path <files>`;
confirm writes with `rtk git diff --stat`. Worktree unit files (`<file>_<task>.go`, folded before merge, never
committed): [lane-orchestrator.md](references/lane-orchestrator.md) → Unit files.

## Rules

- Use the Agent tool with a harness type, or a carried ask from main; long jobs use the External relay contract.
- Decide the owner before gathering evidence. Simplify runs once per diff (`rules/standards/quality.md` step 5).
- Preserve explicit agent/model choices unless authorized; models go on `ask --overlay '{"model":"<m>"}'`.

## Error handling

| Situation | Strategy |
| --- | --- |
| One leaf fails · budget ended | R34: main finishes partial work without resending; takes over after 2 failures. |
| Most leaves fail | Stop and tell the owner; the brief or plan is wrong. |
| Unowned edit | Report it; remove only hunks proven to be the leaf's, preserving pre-existing edits. |
| Block on a leaf | Report the refusal; guard policy: `rules/harness/hooks.md` → `safety-scan`. |
| Missing credential or route | Report BLOCKED with the fix command; no silent in-context substitution. |
| Quota pressure | Preserve model choice; fallback: [cost-routing.md](references/cost-routing.md). |
