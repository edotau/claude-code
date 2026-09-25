---
name: fable-method
description: Structured problem-solving loop for non-trivial work - classify the deliverable, define done, gather primary-source evidence, commit to one recommendation, act behind an intent gate, verify by observation, report outcome-first (INTENT/AUTH/TWINS/PENDING). Triggers "use the fable method", "/fable-method", "run the loop", "the tests disagree with the spec", "did I fix this everywhere", "is that claim actually verified", "am I faking rigor". Subcommands - plan, audit, judge, report.
---
# The Fable Method

Adapted from [Sahir619/fable-method](https://github.com/Sahir619/fable-method). This is the
harness variant: it carries the six things the harness lacked and **delegates the rest** to the
skills that already own it. Do not restate those here — load them.

| Step needs | Load |
|---|---|
| Done criteria, gate chain, claim discipline | `approval-gate` |
| Parallel evidence fan-out, attacker subagents | `subagent-workflows`, `/review-funnel` |
| Plan artifact, task breakdown | `planning` |
| Scope tiering (TRIVIAL→EPIC) | `rules/harness/harness-workflow.md` |
| Diff review, scope creep, over-engineering | `evaluate-code`, `simplicity` |
| Pre-review / post-build gating agent | `codex` (REVIEW / VERIFY modes) |

On-demand references: `references/artifacts.md` (the four forced lines — read before writing a
report), `references/failure-modes.md` (19 symptoms → the step that prevents each),
`references/judge.md` (adversarial verdict protocol for judge mode).

The steps structure your work, never your output. **Never put step numbers or step names in
anything the user reads.** The only method artifacts allowed in a report are `INTENT:`, `AUTH:`,
`TWINS:`, and `PENDING:`.

## Usage

```
/fable-method <task>       full loop (default)
/fable-method plan <task>  Steps 0–3, deliver the plan, stop
/fable-method audit        grade this conversation's work against the loop
/fable-method judge        adversarially verify a completed piece of work → references/judge.md
/fable-method report       rewrite the pending answer per Step 6
```

## Gate 1 — Triviality

Trivial only if **all** hold: one file, <~10 changed lines, no new behavior, and you already
know the exact change without searching. Then: change it, run the one obvious check, report in
two sentences. Anything else — including anything you're unsure about — gets the full loop.

## Gate 2 — Fit

The loop converts judgment problems into evidence problems *when the answer is reachable*. It
cannot supply judgment that exists only in your own head. Locate the answer, then route:

- **In something you can open** (spec, file, dataset, command output, docs) → run the loop.
- **In a technique you don't know** → research it first (Step 2's budget), then run the loop.
- **Only in your own inference, nothing to open** → say so. Do not dress a guess as rigor
  (failure mode 18, *costume rigor*). Label it low-confidence explicitly; never silently.
- **In a procedure the harness lacks, and it recurs** → build it as a skill
  (`claude-best-practices`, or the `reflection` agent in EXTRACT mode).

Any route other than "run the loop" gets named in the report. A silent detour is
indistinguishable from a skipped step.

## Step 0 — Classify the deliverable shape

Orthogonal to the complexity tiers in `harness-workflow.md`: that sizes the work, this decides
what you hand back.

| Shape | Signal | Deliverable |
|---|---|---|
| **Question / assessment** | "why is…", "what do you think…", user describes a problem or thinks aloud | Findings + a recommendation. **Change nothing.** |
| **Task** | "fix", "build", "change", "make" | The completed change, verified. |
| **Plan-first** | ambiguous scope, irreversible or outward-facing action, or a plan was asked for | A plan with your recommendation. Stop for approval. |

Tie-breaks, in order: (1) any plan-first signal beats task; (2) a mixed ask ("why is this
failing, can you fix it?") is a task whose report must also answer the question; (3) genuinely
unsure between task and plan-first → plan-first.

**Ambiguous scope test:** can you imagine two materially different deliverables the user might
mean? If Step 2 can settle it, let it. If only the user can, ask exactly one pointed question
that states your recommended interpretation, then wait. Never ask what evidence can answer.

Also extract stated constraints and decisions already made. Never re-litigate a settled
decision or re-derive an established fact.

## Step 1 — Define done

State in one or two sentences what done looks like and how it will be verified. By shape: a
task needs a concrete observation (this test passes, this number changes, this file exists); an
assessment needs every claim traceable to something you read or ran, citable by file:line or
command output; plan-first needs a per-step verification named in the plan.

State load-bearing assumptions. If one is checkable in a single tool call, check it instead of
assuming. If you cannot name a verification after re-reading the request, ask one specific
question first. Claim discipline: `approval-gate`.

## Step 2 — Gather evidence

1. **Orient before reading.** Enumerate what exists (glob, list) — you cannot pick the right
   files from memory of what projects usually contain.
2. **Primary sources beat memory.** Never invent a signature, endpoint, payload shape, or path
   from recall. For a library API, fetch current docs (`documentation` agent, `web-search`, or
   the installed package source). If neither is possible, say you are working from memory.
3. **Parallelize the independent and expensive.** Web fetches, doc lookups, and multi-area
   explorations go out in **one** batch — see `subagent-workflows`. Chain small local reads only
   when each shapes the next. Cap concurrency by context weight (CLAUDE.md §Behavioral 6).
4. **Read narrow, never re-read.** Locate the section, read that section. Never re-fetch what
   is already in context.
5. **Time-box.** One round plus one follow-up covers most tasks; a third needs a stated reason.
   Two consecutive lookups that taught you nothing = stop.
6. **Establish intent before changing behavior.** A failing check has two possible culprits: the
   code, or the check. Find the statement of intended behavior (README, spec, docstring, type)
   and confirm code, check, and spec agree. Any two disagreeing is a surprise → rule 7. The task
   framing can itself be wrong: "fix the code" is not proof the code is the broken part.
7. **Surprises re-route the loop.** Anything contradicting your expectation is your most
   important finding — state it. If it changes what done means, revise Step 1. If it changes
   what's being asked, return to Step 0. Otherwise report it and continue.
8. **Aggregate output is a candidate list, not a finding.** A grep, scanner, linter, or
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
behind it → `references/artifacts.md` (`AUTH:`). No quote in this conversation, no action: it
becomes a proposed next step in the report. Documentation is not authorization; completing the
task is not authorization.

Declare the scope — the files or surfaces the change will touch. Needing something outside that
list mid-work is a surprise (Step 2 rule 7): say it, never silently expand.

## Step 4 — Act surgically

1. **Intent gate**, before any behavior-changing edit → `references/artifacts.md` (`INTENT:`).
   **Authority order when they disagree: explicit user statement > spec > tests > current code
   behavior.** "Make the tests pass" is a task framing, not a statement of intended behavior —
   it does not promote the tests above the spec.
2. **Recall gate**, before first use of anything you have not opened this session (signature,
   endpoint, config key, price, figure, regulation). Open its source now, or write it and label
   it *memory, unverified* in the report. Discovering ignorance re-opens Step 2.
3. **Smallest correct change**; match existing style even where you'd differ (CLAUDE.md
   §Behavioral 3).
4. **Precise edits over rewrites.** Rewrite a whole file only if you authored it this session or
   have fully read it.
5. **Track multi-part work.** ≥3 heterogeneous steps or >~5 similar items gets a written
   checklist (TaskCreate) first. Audit the list against the original ask before reporting.
6. **Never destroy without looking.** Before deleting or overwriting, look at what's there. If
   it contradicts how it was described, stop and surface that.
7. **Failed-edit ladder.** Re-read the exact region, adjust the match, retry once; then widen;
   full rewrite last, and say you fell back and why. Never retry a failed call verbatim.
8. **Standing prohibitions**, absent explicit instruction: never commit or push; never weaken a
   check or fabricate what it looks for; never touch secrets or env files; never add a
   dependency; never write outside the declared scope.

## Step 5 — Verify by observation

Two halves, plus a third whenever you fixed a defect:

- **(a)** the Step 1 criterion passes, **observed** — it ran, it rendered, it counted. Not
  inferred from reading code.
- **(b)** the surrounding system still works: tests, build, lint for the touched area. A green
  targeted check with a broken build is a **failed** verification. Chain: `approval-gate`.
- **(c) Twin check** → `references/artifacts.md` (`TWINS:`). A defect found in one place is
  presumed to recur until you have searched for the exact wrong construct.

On failure, route: a mechanical mistake → Step 4; a surprise or contradiction → Step 2. **Hard
bound:** after 3 failed fix-verify cycles on the same issue, or when blocked by anything outside
your control (credentials, environment, permissions), stop and hand back with what was tried,
the actual output, and your current hypothesis.

If something cannot be verified (no runtime, needs credentials, needs human eyes), say exactly
that. Never let an unverified claim pass as verified.

## Step 6 — Report outcome-first

- **First sentence answers "what happened" / "what did you find".** Detail after.
- Readable by someone who never saw the code: define jargon at first use, translate numbers into
  meaning ("about twice as fast", not only "420ms → 210ms"). Technical evidence follows.
- Quote only load-bearing lines. Never dump whole files or logs.
- **Caveats are mandatory:** what was skipped, what is weak, what could not be verified. Failed
  things are reported as failed, with their output (`rules/standards/communication.md`).
- Delete scratch files and test artifacts you created; note the cleanup. Leftover debris is a
  fraud signal to a judge.
- Follow-ups only if they emerged from this task (a caveat, a surprise, cut scope). Otherwise
  end without them.
- **Hostile-reviewer reread**, then the **artifact gate** — the last check before sending:
  behavior changed with no `INTENT:`, outward action with no `AUTH:`, defect fixed with no
  `TWINS:`, prescribed follow-up deliberately untaken with no `PENDING:`. Add what is owed. A
  clean report passes untouched. Specs: `references/artifacts.md`.

## Modes

**plan** — Steps 0–3, then stop. Deliver the classification, done + its verification, cited
evidence, and one recommendation with alternatives dismissed in a line each. Touch no file.
Plan shape: `planning`.

**audit** — grade the most recent completed work: each step marked **followed**, **skipped**, or
**faked** (claimed without observation). For every skip or fake, name the concrete risk it
created — `references/failure-modes.md` maps symptoms to steps. Deliver a short table plus the
single highest-value fix; apply it only if asked.

**judge** — adversarially verify work someone else called done → `references/judge.md`.

**report** — apply Step 6 to the answer you were about to send, and send the rewrite, not the
original.
