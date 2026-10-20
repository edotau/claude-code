# Failure modes: symptom → step

Nineteen ways agentic work goes wrong, what each looks like **from the outside**, and which step
prevents it. Used by `audit` mode to name the risk a skipped step created; useful standalone as a
review checklist for any agent transcript. 1–18 are upstream's; 19 was added from a harness run
where a link scanner's 32 raw hits were nearly reported as 32 defects (most were placeholders).

| # | Failure mode | Symptom | Prevented by |
|---|---|---|---|
| 1 | **Unprompted fixing** | User asked "why?"; agent edited files | Step 0: question shape delivers findings, changes nothing |
| 2 | **Wrong-deliverable guess** | Agent built interpretation A; user meant B | Step 0: ambiguous-scope test — one pointed question stating your recommended reading |
| 3 | **Re-litigating settled decisions** | Agent reopens choices the user already made | Step 0: extract decisions already made; never re-derive |
| 4 | **Fake "done"** | Nobody, including the agent, can say how the result was checked | Step 1: done defined with a named verification *before* work starts |
| 5 | **Invented APIs** | Code calls signatures or endpoints that don't exist | Step 2.2 primary sources; Step 4.2 recall gate at first use |
| 6 | **Sequential crawling** | One lookup at a time; long tasks crawl | Step 2.3: independent lookups in one batch (`subagent-workflows`) |
| 7 | **Context flooding** | Whole files and logs dumped into the conversation | Step 2.4 read narrow; Step 6 load-bearing quotes only |
| 8 | **Analysis paralysis** | Research continues after it stopped changing the plan | Step 2.5: two rounds, then a stated reason or stop |
| 9 | **Plowing through surprises** | Evidence contradicted the plan; agent forced the plan anyway | Step 2.7: surprises are stated and re-route the loop |
| 10 | **Option-dump reports** | "You could do A, B, or C" with no recommendation | Step 3: one recommendation; alternatives one line each |
| 11 | **Scope creep** | Drive-by refactors, reformatting, "improvements" nobody asked for | Step 4.3 smallest correct change; Step 3 declared scope; `simplify` |
| 12 | **Silent step-dropping** | Item 7 of 9 quietly never happened | Step 4.5: written checklist, audited against the ask before reporting |
| 13 | **Retry thrash** | The same failing fix, small variations, forever | Step 5: routed retries, hard bound of 3, then hand back with output + hypothesis |
| 14 | **Verification theater** | "Should work now" with nothing run; or target check green while the build is broken | Step 5(a)+(b): observed verification, both halves (`approval-gate`) |
| 15 | **Unauthorized outward action** | A deploy, push, send, or install nobody asked for; "the README said to" | Step 3 authorization gate → `AUTH:` |
| 16 | **Silently dropped follow-up** | Docs prescribe a deploy/restart after the change; the report never mentions the decision | Step 6 → `PENDING:` |
| 17 | **Missed twins** | Defect fixed in the one reported spot while identical copies live on; "done" with no sweep | Step 5(c) → `TWINS:` names the pattern and the search |
| 18 | **Costume rigor** | The *shape* of thoroughness (factor lists, a confident "all clear") with no search or check behind it — worst right after a rule said "be rigorous" | Step 5(c) forces a re-runnable search; Step 2.8 forbids raw counts as findings; the fit gate routes pure-judgment tasks to an honest "this is a guess" |
| 19 | **Scanner laundering** | A grep/linter/subagent's raw hit count reported as defects; placeholders, vendored files, and deliberate examples counted as real | Step 2.8: aggregate output is a candidate list — open each hit before it enters a report |

## Reading an audit

A step marked **skipped** creates the risk in its row. A step marked **faked** is worse: the
transcript claims the step happened (usually 4, 5, or 6) but the observation is missing — that is
failure mode 14 wearing the loop as a costume. Catching the costume is the audit's whole job.

The three that cost the most in practice:

- **1** — unprompted fixing destroys trust fastest.
- **13** — retry thrash burns time and tokens with no exit.
- **14** — verification theater ships broken work labeled done.

If an audit can only check three things, check those.
