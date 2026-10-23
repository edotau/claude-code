# Step detail

Read the relevant section alongside the first rule in [the skill](../SKILL.md).

## Step 0 — Classify the deliverable shape

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

By shape: a
task needs a concrete observation (this test passes, this number changes, this file exists); an
assessment needs every claim traceable to something you read or ran, citable by file:line or
command output; plan-first needs a per-step verification named in the plan.

State load-bearing assumptions. If one is checkable in a single tool call, check it instead of
assuming. If you cannot name a verification after re-reading the request, ask one specific
question first. Claim discipline: `approval-gate`.

## Step 2 — Gather evidence

2. **Primary sources beat memory.** Never invent a signature, endpoint, payload shape, or path
   from recall. For a library API, fetch current docs (`opencode` agent in DOC-LOOKUP mode, the
   Context7 MCP, `web-search`, or the installed package source). If none is reachable, say you
   are working from memory.
3. **Parallelize the independent and expensive.** Web fetches, doc lookups, and multi-area
   explorations go out in **one** batch — see `subagent-workflows`. Chain small local reads only
   when each shapes the next. Cap concurrency by context weight (CLAUDE.md §Behavioral 8).
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

2. **Recall gate**, before first use of anything you have not opened this session (signature,
   endpoint, config key, price, figure, regulation). Open its source now, or write it and label
   it *memory, unverified* in the report. Discovering ignorance re-opens Step 2.
3. **Smallest correct change**; match existing style even where you'd differ (CLAUDE.md
   §Behavioral 3).
4. **Precise edits over rewrites.** Rewrite a whole file only if you authored it this session or
   have fully read it.
5. **Track multi-part work.** ≥3 heterogeneous steps or >~5 similar items gets a written
   checklist first (the harness's todo tool if one exists, else a list in your reply). Audit the
   list against the original ask before reporting.
6. **Never destroy without looking.** Before deleting or overwriting, look at what's there. If
   it contradicts how it was described, stop and surface that.
7. **Failed-edit ladder.** Re-read the exact region, adjust the match, retry once; then widen;
   full rewrite last, and say you fell back and why. Never retry a failed call verbatim.
8. **Standing prohibitions**, absent explicit instruction: never commit or push; never weaken a
   check or fabricate what it looks for; never touch secrets or env files; never add a
   dependency; never write outside the declared scope.

## Step 5 — Verify by observation

- **(b)** the surrounding system still works: tests, build, lint for the touched area. A green
  targeted check with a broken build is a **failed** verification. Chain: `approval-gate`.
- **(c) Twin check** → `references/artifacts.md` (`TWINS:`). A defect found in one place is
  presumed to recur until you have searched for the exact wrong construct.

Code instantiation (live first, tests with teeth): `skills/testing-guidelines/references/test-discipline.md`.

On failure, route: a mechanical mistake → Step 4; a surprise or contradiction → Step 2. **Hard
bound:** after 3 failed fix-verify cycles on the same issue, or when blocked by anything outside
your control (credentials, environment, permissions), stop and hand back with what was tried,
the actual output, and your current hypothesis.

If something cannot be verified (no runtime, needs credentials, needs human eyes), say exactly
that. Never let an unverified claim pass as verified.

## Step 6 — Report outcome-first

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
