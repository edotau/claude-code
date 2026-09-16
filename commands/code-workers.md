---
description: "Fan out code-workers subagents to finish a series of tasks — parallel (independent), sequential (plan with review gate per task), or auto. The multi-leaf counterpart to a single Agent dispatch."
argument-hint: "[parallel | sequential | auto] <task1 | task2 | ...>  (tasks '|'-separated; blank reads the approved plan)"
allowed-tools: Bash, Read, Grep, Glob, Edit, Write, Agent
---

The multi-leaf shortcut for subagent fan-out. Where a single `Agent` call dispatches **one**
agent, this fans out **many** `code-workers` leaves to finish a series of jobs. Each
leaf is the **code-workers** agent (Sonnet-pinned, cannot fan out further — it does its one task
in-context and reports back). This is the ergonomic front door to the `subagent-workflows` skill's two
strategies: `dispatching-parallel-agents` (concurrent) and `subagent-driven-development` (sequential
with per-task review).

## Mode (first arg, optional)

| Arg | Shape | What runs |
|-----|-------|-----------|
| `parallel` | concurrent fan-out | One `code-workers` leaf per task, **all dispatched in a single message** so they run concurrently, each scoped to disjoint files in the shared tree. Per `subagent-workflows/references/dispatching-parallel-agents.md`. |
| `sequential` | one-at-a-time + review | One leaf per task in order; after each, a review gate (step 4) before the next. Per `subagent-workflows/references/subagent-driven-development.md`. |
| *(blank)* or `auto` | infer | Independent tasks / no shared files → parallel; ordered or coupled tasks, or reading a plan → sequential. State the chosen shape and why before dispatching. |

The remaining args are the tasks: a `|`-separated list, or blank to read the approved plan.

## 1. Parse mode + tasks

Strip an optional leading mode word from `$ARGUMENTS`, then split the rest on `|` into a task list.

```bash
ARGS="$ARGUMENTS"
MODE=auto
case "$ARGS" in
  parallel*)   MODE=parallel;   ARGS="${ARGS#parallel}";;
  sequential*) MODE=sequential; ARGS="${ARGS#sequential}";;
  auto*)       MODE=auto;       ARGS="${ARGS#auto}";;
esac
ARGS="${ARGS#"${ARGS%%[![:space:]]*}"}"   # ltrim
echo "mode=$MODE"
echo "tasks (| -separated): $ARGS"
```

If the task list is empty, read the approved plan — the most recent
`$HOME/.claude/docs/plans/*.md` — and extract its task breakdown as the task list.

```bash
[ -z "$ARGS" ] && ls -t "$HOME/.claude/docs/plans/"*.md 2>/dev/null | head -1
```

State the resolved task list before dispatching. For `auto`, decide the shape (independent → parallel;
coupled/ordered/plan-derived → sequential) and say why in one line.

## 2. Build a self-contained handoff per task

You start each leaf cold — it inherits none of this session. For every task, construct the handoff
contract from `subagent-workflows/references/choosing-a-pattern.md`:

- **Objective** — what to build / review / investigate.
- **Scope boundary** — the exact files this leaf may touch (so parallel leaves don't collide).
- **Upstream artifacts** — relevant paths, prior findings, the plan section.
- **Acceptance criteria** — how the leaf knows it's done.
- **Verification command** — the exact `make`/`pytest`/script to run before reporting done.
- **Skills to read** *(optional)* — `skills/<name>/SKILL.md` paths when the task needs a method
  (e.g. `evaluate-code` for a review leaf, `testing` for a test leaf).

## 3. Dispatch

Dispatch each task via the **Agent** tool with `subagent_type: "code-workers"`, passing only the
handoff (NOT this session's history).

- **parallel** — emit **all** `Agent` calls in a **single message** so they run concurrently. Concurrency:
  lean leaves ~8–10 in flight, deep-context leaves ≤4 (input-tokens/min is the binding cap). Agents share the tree on disk — scope each
  handoff to disjoint files so concurrent writers don't collide.
- **sequential** — dispatch **one** leaf, then run the review gate (step 4) before the next task.
  **Never** run two implementer leaves in parallel here — coupled tasks would edit shared code (the
  skill's red-flag).

If a leaf reports the task is too big to do in-context (it cannot fan out further), split the task and
re-dispatch — don't ask the leaf to delegate.

## 4. Review gate (sequential mode)

After each sequential leaf, verify before proceeding — a spec-compliance pass then a code-quality
pass, reusing the templates in `subagent-workflows/references/` (`spec-reviewer-prompt.md`,
`code-quality-reviewer-prompt.md`), or `/code-review` over the leaf's diff. Loop
fixes until the gate passes, then move to the next task. Do not start the next task on a red gate.

## 5. Synthesize

Collect each leaf's contract: what it did, files changed, verification result (pass/fail + output),
and anything surprising (this is how you detect hidden coupling across leaves). Present one
consolidated report:

- Per-task status table (task · leaf verdict · files · verification).
- Overall verdict.
