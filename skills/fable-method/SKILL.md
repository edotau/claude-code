---
name: fable-method
description: 'Structured problem-solving loop - classify, define done, evidence, one recommendation, intent gate, verify by observation, outcome-first report (INTENT/AUTH/TWINS/PENDING). Triggers "use the fable method", "/fable-method", "run the loop", "the tests disagree with the spec", "did I fix this everywhere", "is that claim actually verified". Subcommands - plan, audit, judge, report.'
metadata:
  version: 1.1.0
---

# The Fable Method

Adapted from [Sahir619/fable-method](https://github.com/Sahir619/fable-method). This is the
harness variant: it carries the six things the harness lacked and **delegates the rest** to the
skills that already own it. Do not restate those here — load them.

| Step needs | Load |
|---|---|
| Done criteria, gate chain, claim discipline | `approval-gate` |
| Parallel evidence fan-out, attacker subagents | `subagent-workflows`, Workflow `review-funnel` |
| Plan artifact, task breakdown | `Plan` agent (`agents/Plan.md`) |
| Scope tiering (TRIVIAL→EPIC) | `rules/workflow/master-workflow.md` |
| Diff review, scope creep, detectors | `evaluate-code`, `simplify`, `security-reviewer` |
| Pre-review / post-build gating agent | `codex` (REVIEW / VERIFY modes) |

References: `references/artifacts.md` (the four forced lines — read before any report),
`references/failure-modes.md` (19 symptoms → step), `references/judge.md` (judge protocol, audit
rubric), `references/eval.md` (upstream parity; the fixture a rule change must bring).

The steps structure your work, never your output. **Never put step numbers or step names in
anything the user reads.** The only method artifacts allowed in a report are `INTENT:`, `AUTH:`,
`TWINS:`, and `PENDING:`. Audit output is exempt: it grades the method, not the work.

## Usage

```text
/fable-method <task>       full loop (default)
/fable-method plan <task>  Steps 0–3, deliver the plan, stop
/fable-method audit        grade this conversation's work against the loop
/fable-method judge        adversarially verify a completed piece of work → references/judge.md
/fable-method report       rewrite the pending answer per Step 6
```

## Gate 1 — Triviality

Trivial only if **all** hold: one file, <~10 changed lines, no new behavior, and you already
know the exact change without searching. Then: change it, run the one obvious check, report in
two sentences. Anything else — or any doubt — gets the full loop.

## Gate 2 — Fit

The loop turns judgment into evidence only when the answer is reachable. Locate it, then route:

- **In something you can open** (spec, file, dataset, command output, docs) → run the loop.
- **In a technique you don't know** → research it first (Step 2's budget), then run the loop.
- **Only in your own inference, nothing to open** → say so. Do not dress a guess as rigor
  (failure mode 18, costume rigor). Label it low-confidence, never silently.
- **In a procedure the harness lacks, and it recurs** → build it as a skill (`/reflection:promote --to skill`).

Any route other than "run the loop" is named in the report; a silent detour reads as a skipped step.

## Step 0 — Classify the deliverable shape

Orthogonal to the complexity tiers in `rules/workflow/master-workflow.md`: that sizes the work, this decides
what you hand back.

[Shapes and scope test](references/steps.md#step-0--classify-the-deliverable-shape).

## Step 1 — Define done

State in one or two sentences what done looks like and how it will be verified.

[Verification and assumptions](references/steps.md#step-1--define-done).

## Step 2 — Gather evidence

1. **Orient before reading.** Enumerate what exists (glob, list) — you cannot pick the right
   files from memory of what projects usually contain. The README or spec of the directory you will
   touch is always in the minimum evidence set: read-narrow without it yields safe outcomes by
   blindness, not discipline.

[Sources, budget, surprises](references/steps.md#step-2--gather-evidence).

## Step 3 — Decide and commit

Synthesize into **one** recommendation. Alternatives you seriously weighed get one line each
saying why they lost; if you weighed none, say nothing. No option-dumps.

[Authorization and scope](references/steps.md#step-3--decide-and-commit).

## Step 4 — Act surgically

1. **Intent gate**, before any behavior-changing edit → `references/artifacts.md` (`INTENT:`).
   **Authority order when they disagree: explicit user statement > spec > tests > current code
   behavior.** "Make the tests pass" is a task framing, not a statement of intended behavior —
   it does not promote the tests above the spec.

[Edit gates and prohibitions](references/steps.md#step-4--act-surgically).

## Step 5 — Verify by observation

Two halves, plus a third whenever you fixed a defect:

- **(a)** the Step 1 criterion passes, **observed** — it ran, it rendered, it counted. Not
  inferred from reading code.

[Checks and retry bound](references/steps.md#step-5--verify-by-observation).

## Step 6 — Report outcome-first

- **First sentence answers "what happened" / "what did you find".** Detail after.

[Caveats and artifact gate](references/steps.md#step-6--report-outcome-first).

## Modes

**plan** — Steps 0–3, then stop: classification, done + its verification, cited evidence, one
recommendation with alternatives dismissed in a line each. Touch no file. Shape: `agents/Plan.md`.

**audit** — grade the most recent completed work: each step marked **followed**, **skipped**, or
**faked** (claimed without observation). For every skip or fake, name the concrete risk it
created — `references/failure-modes.md` maps symptoms to steps. Score it on the 0–2 rubric in
`references/judge.md` (run the detectors first). Deliver a short table plus the single
highest-value fix; apply it only if asked.

**judge** — adversarially verify work someone else called done → `references/judge.md`.

**report** — apply Step 6 to the answer you were about to send, and send the rewrite, not the
original.
