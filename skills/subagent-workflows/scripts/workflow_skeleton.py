#!/usr/bin/env python3
"""Emit a runnable `Workflow`-tool JS skeleton for a chosen orchestration pattern.

Rewrite of the upstream agent-workflow-designer's JSON scaffolder: instead of a config for an
engine we don't run, this prints a starting script for the harness `Workflow` tool (the thing
that actually orchestrates here — see references/choosing-a-pattern.md). Fill in the prompts,
paste into a Workflow call, iterate. Stdlib-only.

Usage:
  workflow_skeleton.py pipeline    --name review-changes
  workflow_skeleton.py parallel    --name fan-out-audit
  workflow_skeleton.py evaluator   --name gen-verify
  workflow_skeleton.py orchestrator --name plan-then-build
"""

from __future__ import annotations

import argparse
import sys

META = """export const meta = {{
  name: '{name}',
  description: '{desc}',
  phases: [{phases}],
}}"""


def pipeline(name: str) -> str:
    # No barrier between stages: each item verifies as soon as its review completes.
    return (
        META.format(
            name=name,
            desc="Pipeline: each item through ordered stages, no barrier",
            phases="{ title: 'Work' }, { title: 'Verify' }",
        )
        + """

const ITEMS = args ?? []   // pass the work-list via Workflow `args`
const results = await pipeline(
  ITEMS,
  (item) => agent(`Do the work for: ${JSON.stringify(item)}`, {phase: 'Work', schema: WORK_SCHEMA}),
  (work, item) => agent(`Adversarially verify: ${work.summary}`, {phase: 'Verify', schema: VERDICT_SCHEMA})
    .then(v => ({...work, verdict: v})),
)
return results.filter(Boolean).filter(r => r.verdict?.ok)"""
    )


def parallel(name: str) -> str:
    # Barrier: use ONLY when the next step needs all prior results together (dedup/merge/early-exit).
    return (
        META.format(
            name=name,
            desc="Parallel fan-out with a barrier, then synthesize",
            phases="{ title: 'Fan-out' }, { title: 'Synthesize' }",
        )
        + """

const TASKS = args ?? []
phase('Fan-out')
const found = (await parallel(TASKS.map(t => () =>
  agent(`Investigate: ${JSON.stringify(t)}`, {schema: FINDING_SCHEMA})))).filter(Boolean)
// Barrier justified: synthesis needs the FULL set (dedup/merge across all findings).
phase('Synthesize')
return await agent(`Synthesize these findings into one report: ${JSON.stringify(found)}`)"""
    )


def evaluator(name: str) -> str:
    return (
        META.format(
            name=name,
            desc="Generate, then adversarially verify before accepting",
            phases="{ title: 'Generate' }, { title: 'Verify' }",
        )
        + """

phase('Generate')
const draft = await agent('Produce the artifact.', {schema: DRAFT_SCHEMA})
phase('Verify')
// N independent skeptics, each prompted to REFUTE; accept only if a majority can't.
const votes = (await parallel(['correctness', 'security', 'repro'].map(lens => () =>
  agent(`Try to refute the ${lens} of: ${draft.summary}. Default refuted=true if unsure.`,
        {schema: VERDICT_SCHEMA})))).filter(Boolean)
const accepted = votes.filter(v => !v.refuted).length >= 2
return {draft, accepted, votes}"""
    )


def orchestrator(name: str) -> str:
    return (
        META.format(
            name=name,
            desc="Planner decomposes, then pipeline over the work-list",
            phases="{ title: 'Plan' }, { title: 'Build' }",
        )
        + """

phase('Plan')
// Discover the work-list at runtime, then fan out over it (don't hardcode the shape).
const plan = await agent(`Break down this goal into parallel-safe tasks: ${args?.goal ?? ''}`,
                         {schema: PLAN_SCHEMA})
phase('Build')
// isolation:'worktree' ONLY if tasks mutate files in parallel (else drop it — it's expensive).
const done = await pipeline(
  plan.tasks,
  (task) => agent(task.brief, {phase: 'Build', isolation: 'worktree', schema: RESULT_SCHEMA}),
  (result) => agent(`Review for spec + quality: ${result.summary}`, {phase: 'Build', schema: REVIEW_SCHEMA})
    .then(r => ({...result, review: r})),
)
return done.filter(Boolean)"""
    )


PATTERNS = {"pipeline": pipeline, "parallel": parallel, "evaluator": evaluator, "orchestrator": orchestrator}

NOTE = """
// ── Fill in before running ──
//  • Replace *_SCHEMA with JSON Schemas (or drop the {schema:...} opt for free-text agents).
//  • Every agent() payload needs a self-contained handoff (objective + constraints + artifacts
//    + acceptance + verify command) — see references/choosing-a-pattern.md. Cold-start test:
//    a fresh agent with zero context can execute it.
//  • Pipeline is the default (no barrier). Use parallel() only when a step needs ALL prior results.
//  • 'router' isn't a Workflow shape — it's an inline branch on the input, then dispatch. No skeleton.
"""


def main() -> int:
    ap = argparse.ArgumentParser(description="Emit a Workflow-tool JS skeleton for a pattern.")
    ap.add_argument("pattern", choices=sorted(PATTERNS), help="orchestration shape")
    ap.add_argument("--name", default="new-workflow", help="workflow name (letters/digits/dashes)")
    args = ap.parse_args()
    print(PATTERNS[args.pattern](args.name))
    print(NOTE)
    return 0


if __name__ == "__main__":
    sys.exit(main())
