---
name: problem-solving
description: 'Doing the work: the evidence loop for a task, fix or plan - classify the deliverable, define done, gather evidence, one recommendation, act surgically behind TASK/AUTH notes, verify by observation with an ISSUE sweep for copies, report outcome-first in plain prose. Triggers "/problem-solving", "run the loop", "the tests disagree with the spec", "did I fix this everywhere", "plan this first", "act on review feedback", "verify before done", "run the gates", "are we done". Modes - plan, report.'
metadata:
  version: 2.0.0
---

# Problem Solving

Derived from [Sahir619/fable-method](https://github.com/Sahir619/fable-method) (MIT).

**Forced lines — write each in your working notes at its moment, every time:**

1. Before any behavior-changing edit, open the README/spec first: `TASK: code does <X>; the failing check
   expects <Y>; the spec says <Z>`. Y ≠ Z → do not change the code to match the check.
2. After fixing a defect, before reporting: `ISSUE: searched <pattern> — found <N> other sites: <files|none>`.
3. Before reporting: `PENDING: <follow-up a README/runbook/skill prescribed that you did not take, or "none">`.

The steps structure your work, never your output: **no step numbers or names in anything the user reads.** Report
only the facts `TASK:`, `AUTH:`, `ISSUE:`, `PENDING:` stand for, as plain prose, never the raw tag
(`references/notes.md` — read before any report).

| Step needs | Load |
|---|---|
| Done criteria, gate chain, claim discipline | `references/verify.md` |
| Acting on review feedback | `references/feedback.md` |
| Plan artifact, task breakdown | `Plan` agent (`agents/Plan.md`) |
| Scope tiering (TRIVIAL→EPIC) | `rules/workflows/master-workflow.md` |
| Detectors over a plan | `claude-code review goals\|assumptions` |

## Usage

```text
/problem-solving <task>       full loop (default)
/problem-solving plan <task>  Steps 0-3, deliver the plan, stop
/problem-solving report       rewrite the pending answer per Step 6
```

## Gate 1 — Triviality

Trivial only if **all** hold: one file, <~10 changed lines, no new behavior, exact change known. Then change it, run
the one check, report in two sentences. Any doubt: full loop.

## Gate 2 — Fit

Route by where the answer lives; name any route other than "run the loop" in the report.

- Something you can open (spec, file, output, docs) → run the loop.
- A technique you don't know → research it (Step 2's budget), then loop.
- Only your own inference → say so, label it low-confidence (failure mode 18, costume rigor).
- A recurring procedure the harness lacks → `claude-code memory promote --to skill`.

## Step 0 — Classify the deliverable shape

| Shape | Signal | Deliverable |
|---|---|---|
| **Question / assessment** | "why is…", "what do you think…", user thinks aloud | Findings + a recommendation. **Change nothing.** |
| **Task** | "fix", "build", "change", "make" | The completed change, verified. |
| **Plan-first** | ambiguous scope, irreversible/outward-facing action, or a plan was asked for | A plan with your recommendation. Stop for approval. |

Tie-breaks: (1) any plan-first signal beats task; (2) a mixed ask ("why is this failing, can you fix it?") is a task
whose report also answers the question; (3) unsure → plan-first.

**Ambiguous scope test:** two materially different deliverables imaginable? Let Step 2 settle it if it can; else ask
one pointed question stating your recommended reading, then wait. Never ask what evidence answers, nor about a
reversible edit the spec settles (make it under a stated assumption). Wait only before an irreversible or
outward-facing act (Step 3). Never re-litigate stated constraints or settled decisions.

## Step 1 — Define done

Task: a concrete observation (this test passes, this number changes). Assessment: every claim citable by file:line
or command output. Plan-first: a per-step verification. State load-bearing assumptions; check one if a single tool
call can. No nameable verification → ask one question first.

## Step 2 — Gather evidence

1. **Orient first.** Enumerate what exists; the README or spec of the directory you will touch is always read.
2. **Primary sources beat memory; read narrow, never re-read.** Never invent a signature, endpoint, payload shape or
   path from recall: fetch current docs (`opencode` agent in DOC-LOOKUP mode, Context7 MCP, `web-explorer`, installed
   source) or say none is reachable.
3. **Parallelize the independent and expensive** (fetches, lookups, explorations) in **one** batch; chain local reads
   only when each shapes the next. Cap by context weight (subagent-workflows → Concurrency).
4. **Time-box.** One round plus one follow-up; a third needs a stated reason; two fruitless lookups = stop.
5. **Establish intent before changing behavior.** The culprit is the code or the check: find the intended behavior
   (README, spec, docstring, type); any two of code, check, spec disagreeing is a surprise → rule 6.
6. **Surprises re-route the loop.** State any contradiction of your expectation: it changes what done means → Step 1;
   what's asked → Step 0; otherwise report and continue.
7. **Aggregate output is a candidate list, not a finding.** Open each grep/scanner/linter/subagent hit before it
   enters a report (placeholders, vendored files and examples are noise); a raw count is costume rigor.

## Step 3 — Decide and commit

Synthesize **one** recommendation; alternatives you seriously weighed get one line each on why they lost.

**Reversibility test.** Outward-facing = another person or system can observe it before you could undo it (push,
publish, send, deploy, delete shared data, payment, permission change). Local edits are reversible.

**Authorization gate.** Such an action needs the user's own words (`references/notes.md`, `AUTH:`); no quote in this
conversation, no action: propose it as a next step.

Declare the scope (files or surfaces touched); needing more mid-work is a surprise (Step 2 rule 6).

## Step 4 — Act surgically

1. Write the forced `TASK:` line (spec and authority order: `references/notes.md`). Y and Z disagree → fix the side
   the spec contradicts or stop and report; never edit code to match the check.
2. **Recall gate**, before first use of anything not opened this session (signature, endpoint, config key, price,
   figure, regulation): open its source, or label it *memory, unverified*.
3. **Smallest correct change** (`rules/standards/principles.md`). Rewrite a whole file only if you authored it this
   session or have fully read it. Before deleting or overwriting, look at what's there; if it contradicts its
   description, stop and surface that.
4. **Track multi-part work**: ≥3 heterogeneous steps or >~5 similar items get a written checklist; audit it against
   the ask before reporting.
5. **Failed-edit ladder.** Re-read the region, adjust the match, retry once; then widen; full rewrite last, saying
   why. Never retry a failed call verbatim.
6. **Standing prohibitions**, absent explicit instruction: never commit or push, weaken a check or fabricate what it
   looks for, touch secrets or env files, add a dependency, or write outside the declared scope.

## Step 5 — Verify by observation

Iron law and gate chain: `references/verify.md`. Three parts:

- **(a)** the Step 1 criterion passes, **observed** (it ran, it rendered, it counted), not inferred from reading code.
- **(b)** the surrounding system still works (tests, build, lint of the touched area); a green targeted check with a
  broken build **fails**.
- **(c)** after fixing a defect, write the forced `ISSUE:` sweep before reporting it fixed (`references/notes.md`).

Code: live first, then tests with teeth (`rules/standards/testing-guidelines.md`).

On failure: a mechanical mistake → Step 4; a surprise → Step 2. **Hard bound:** after 3 failed fix-verify cycles on
one issue, or when blocked outside your control (credentials, environment, permissions), stop and hand back with what
was tried, the actual output and your hypothesis. Say exactly what cannot be verified.

## Step 6 — Report outcome-first

No method scaffolding; each forced note becomes its plain sentence (`references/notes.md`).

- Outcome-first, caveats mandatory, failures with their output: `rules/standards/principles.md`.
- Delete scratch files and test artifacts you created; note the cleanup.
- Write the forced `PENDING:` line every time, even for "none"; a named follow-up goes in the report as a next step
  awaiting the user's go, naming who prescribed it. Other follow-ups only if they emerged from this task.
- **Hostile-reviewer reread**, then the **artifact gate** (last check before sending): behavior changed with no
  `TASK:` fact, outward action with no `AUTH:` fact, defect fixed with no `ISSUE:` sweep, prescribed follow-up
  untaken with no `PENDING:` fact. Add what is owed (`references/notes.md`).

## Modes

**plan** — Steps 0–3, then stop: classification, done + verification, cited evidence, one recommendation with
alternatives dismissed in a line each. Touch no file. Shape: `agents/Plan.md`.

**report** — apply Step 6 to the answer you were about to send, and send the rewrite.
