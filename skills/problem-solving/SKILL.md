---
name: problem-solving
description: 'Doing the work: the evidence loop for a task, fix or plan - classify the deliverable, define done, gather evidence, one recommendation, act surgically behind TASK/AUTH notes, verify by observation with an ISSUE sweep for copies, report outcome-first in plain prose. Triggers "/problem-solving", "run the loop", "the tests disagree with the spec", "did I fix this everywhere", "plan this first", "act on review feedback", "verify before done", "run the gates", "are we done". Modes - plan, report.'
metadata:
  version: 2.0.0
---

# Problem Solving

Derived from [Sahir619/fable-method](https://github.com/Sahir619/fable-method) (MIT). The evidence
loop for doing a task, a fix, or a plan — classify it, define done, gather evidence, commit to one
recommendation, act behind forced notes, verify by observation, report outcome-first.

**Forced lines — write each in your working notes at its moment, every time:**

1. Before any behavior-changing edit, open the README/spec first: `TASK: code does <X>; the failing check
   expects <Y>; the spec says <Z>`. Y ≠ Z → do not change the code to match the check.
2. After fixing a defect, before reporting: `ISSUE: searched <pattern> — found <N> other sites: <files|none>`.
3. Before reporting: `PENDING: <follow-up a README/runbook/skill prescribed that you did not take, or "none">`.

The report states each as a plain sentence, never the raw tag (`references/notes.md`).

| Step needs | Load |
|---|---|
| Done criteria, gate chain, claim discipline | `references/verify.md` |
| Acting on review feedback | `references/feedback.md` |
| Plan artifact, task breakdown | `Plan` agent (`agents/Plan.md`) |
| Scope tiering (TRIVIAL→EPIC) | `rules/workflow/master-workflow.md` |
| A recurring missing procedure | `claude-code memory promote --to skill` |
| Assumption/goal detectors over a plan | `claude-code review goals\|assumptions` |

References: `references/notes.md` (the four forced notes — read before any report).

The steps structure your work, never your output. **Never put step numbers or step names in
anything the user reads.** The only method artifacts allowed in a report are `TASK:`, `AUTH:`,
`ISSUE:`, and `PENDING:` — and only as the plain-prose fact they stand for, never the raw tag.

## Usage

```text
/problem-solving <task>       full loop (default)
/problem-solving plan <task>  Steps 0-3, deliver the plan, stop
/problem-solving report       rewrite the pending answer per Step 6
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
- **In a procedure the harness lacks, and it recurs** → build it as a skill (`claude-code memory
  promote --to skill`).

Any route other than "run the loop" is named in the report; a silent detour reads as a skipped step.

## Step 0 — Classify the deliverable shape

Orthogonal to the complexity tiers in `rules/workflow/master-workflow.md`: that sizes the work, this
decides what you hand back.

| Shape | Signal | Deliverable |
|---|---|---|
| **Question / assessment** | "why is…", "what do you think…", user thinks aloud | Findings + a recommendation. **Change nothing.** |
| **Task** | "fix", "build", "change", "make" | The completed change, verified. |
| **Plan-first** | ambiguous scope, irreversible/outward-facing action, or a plan was asked for | A plan with your recommendation. Stop for approval. |

Tie-breaks, in order: (1) any plan-first signal beats task; (2) a mixed ask ("why is this
failing, can you fix it?") is a task whose report must also answer the question; (3) genuinely
unsure between task and plan-first → plan-first.

**Ambiguous scope test:** can you imagine two materially different deliverables the user might
mean? If Step 2 can settle it, let it. If only the user can, ask exactly one pointed question
that states your recommended interpretation, then wait. Never ask what evidence can answer. A question
is for a decision, never for a reversible edit: when the spec settles it, make the change under the
stated assumption and report it; wait only before an irreversible or outward-facing act (Step 3).

Also extract stated constraints and decisions already made. Never re-litigate a settled decision
or re-derive an established fact.

## Step 1 — Define done

By shape: a task needs a concrete observation (this test passes, this number changes, this file
exists); an assessment needs every claim traceable to something you read or ran, citable by
file:line or command output; plan-first needs a per-step verification named in the plan.

State load-bearing assumptions. If one is checkable in a single tool call, check it instead of
assuming. If you cannot name a verification after re-reading the request, ask one specific
question first. Claim discipline: `references/verify.md`.

## Step 2 — Gather evidence

1. **Orient before reading.** Enumerate what exists (glob, list) — you cannot pick the right
   files from memory of what projects usually contain. The README or spec of the directory you
   will touch is always in the minimum evidence set: read-narrow without it yields safe outcomes
   by blindness, not discipline.
2. **Primary sources beat memory, read narrow, never re-read.** Never invent a signature,
   endpoint, payload shape, or path from recall — fetch current docs (`opencode` agent in
   DOC-LOOKUP mode, Context7 MCP, `web-search`, or the installed package source); say so if none
   is reachable. Locate the section, read that section; never re-fetch what's already in context.
3. **Parallelize the independent and expensive.** Web fetches, doc lookups, and multi-area
   explorations go out in **one** batch. Chain small local reads only
   when each shapes the next. Cap concurrency by context weight (CLAUDE.md §Behavioral 8).
4. **Time-box.** One round plus one follow-up covers most tasks; a third needs a stated reason.
   Two consecutive lookups that taught you nothing = stop.
5. **Establish intent before changing behavior.** A failing check has two possible culprits: the
   code, or the check. Find the statement of intended behavior (README, spec, docstring, type)
   and confirm code, check, and spec agree. Any two disagreeing is a surprise → rule 6. The task
   framing can itself be wrong: "fix the code" is not proof the code is the broken part.
6. **Surprises re-route the loop.** Anything contradicting your expectation is your most
   important finding — state it. If it changes what done means, revise Step 1. If it changes
   what's being asked, return to Step 0. Otherwise report it and continue.
7. **Aggregate output is a candidate list, not a finding.** A grep, scanner, linter, or
   subagent's hits are leads: open each one before it enters a report. Expect placeholders
   (`<slug>.md`), vendored third-party files, and intentional examples among the noise. Reporting
   a raw count as defects is costume rigor (failure mode 18) even when the tool ran correctly.

## Step 3 — Decide and commit

Synthesize into **one** recommendation. Alternatives you seriously weighed get one line each
saying why they lost; if you weighed none, say nothing. No option-dumps.

**Reversibility test.** An action is irreversible or outward-facing if another person or system
can observe it before you could undo it: push, publish, send, deploy, delete shared data,
payment, permission change. Local working-tree changes are reversible.

**Authorization gate.** An irreversible or outward-facing action needs the user's own words
behind it → `references/notes.md` (`AUTH:`). No quote in this conversation, no action: it becomes
a proposed next step in the report. Documentation is not authorization; completing the task is
not authorization.

Declare the scope — the files or surfaces the change will touch. Needing something outside that
list mid-work is a surprise (Step 2 rule 6): say it, never silently expand.

## Step 4 — Act surgically

1. Write the forced `TASK:` line (box at the top; spec and authority order: `references/notes.md`). Y and Z
   disagree → fix the side the spec contradicts or stop and report; never edit code to match the check.
2. **Recall gate**, before first use of anything you have not opened this session (signature,
   endpoint, config key, price, figure, regulation). Open its source now, or write it and label
   it *memory, unverified* in the report. Discovering ignorance re-opens Step 2.
3. **Smallest correct change, precise edits over rewrites.** Match existing style even where
   you'd differ (CLAUDE.md §Behavioral 3). Rewrite a whole file only if you authored it this
   session or have fully read it. Before deleting or overwriting, look at what's there — if it
   contradicts how it was described, stop and surface that.
4. **Track multi-part work.** ≥3 heterogeneous steps or >~5 similar items gets a written
   checklist first (the harness's todo tool if one exists, else a list in your reply). Audit the
   list against the original ask before reporting.
5. **Failed-edit ladder.** Re-read the exact region, adjust the match, retry once; then widen;
   full rewrite last, and say you fell back and why. Never retry a failed call verbatim.
6. **Standing prohibitions**, absent explicit instruction: never commit or push; never weaken a
   check or fabricate what it looks for; never touch secrets or env files; never add a
   dependency; never write outside the declared scope.

## Step 5 — Verify by observation

Iron law and the gate chain: `references/verify.md`.

Two halves, plus a third whenever you fixed a defect:

- **(a)** the Step 1 criterion passes, **observed** — it ran, it rendered, it counted. Not
  inferred from reading code.
- **(b)** the surrounding system still works: tests, build, lint for the touched area. A green
  targeted check with a broken build is a **failed** verification. Gate sequence, bounded loop,
  claims table: `references/verify.md`.
- **(c)** whenever you fixed a defect, write the forced `ISSUE:` sweep before reporting it fixed
  (`references/notes.md`).

Code instantiation: live first, then tests with teeth (`rules/standards/quality.md` → Testing).

On failure, route: a mechanical mistake → Step 4; a surprise or contradiction → Step 2. **Hard
bound:** after 3 failed fix-verify cycles on the same issue, or when blocked by anything outside
your control (credentials, environment, permissions), stop and hand back with what was tried, the
actual output, and your current hypothesis.

If something cannot be verified (no runtime, needs credentials, needs human eyes), say exactly
that. Never let an unverified claim pass as verified.

## Step 6 — Report outcome-first

**No method scaffolding in the report:** no step numbers, no step names, no raw `TASK:`/`AUTH:`/
`ISSUE:`/`PENDING:` tags. Each forced note becomes the plain sentence it stands for — see
`references/notes.md` for the rendering of each.

- Outcome-first, caveats mandatory, failures reported with their output: `rules/standards/communication.md`.
- Delete scratch files and test artifacts you created; note the cleanup. Leftover debris is a
  fraud signal to a judge.
- **Before reporting, write this line in your working notes:** `PENDING: <a follow-up a README, runbook or
  installed skill prescribed — deploy, push, ship, restart, regenerate — that you did not take, or "none">`.
  Forced every time, even when the answer is "none": absence is invisible unless you force the check. Any
  named follow-up goes in the report as a next step awaiting the user's go, naming who prescribed it.
- Follow-ups only if they emerged from this task (a caveat, a surprise, cut scope). Otherwise end
  without them.
- **Hostile-reviewer reread**, then the **artifact gate** — the last check before sending: behavior
  changed with no `TASK:` fact stated, outward action with no `AUTH:` fact stated, defect fixed
  with no `ISSUE:` sweep stated, prescribed follow-up untaken with no `PENDING:` fact stated. Add
  what is owed. A clean report passes untouched. Specs: `references/notes.md`.

## Modes

**plan** — Steps 0–3, then stop: classification, done + its verification, cited evidence, one
recommendation with alternatives dismissed in a line each. Touch no file. Shape: `agents/Plan.md`.

**report** — apply Step 6 to the answer you were about to send, and send the rewrite, not the
original.
