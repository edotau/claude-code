# Failure modes: symptom → step

Use this catalogue to name a skipped step's risk. Modes 1–18: fable-method (MIT); 19: harness scanner audit.
Prevention: `../../problem-solving/SKILL.md` and `../../problem-solving/references/verify.md`; fixtures: `eval.md`.

| # | Failure / symptom | Prevention |
| --- | --- | --- |
| 1 | Unprompted fixing: edits after “why?” | Step 0: question → findings only |
| 2 | Wrong-deliverable guess | Step 0: ask one scope question with recommended reading |
| 3 | Re-litigating settled decisions | Step 0: extract, never reopen settled choices |
| 4 | Fake “done”: no named check | Step 1: define verification before work; goal-driven lens |
| 5 | Invented APIs/signatures | Steps 2.2, 4.2: primary sources and recall at first use |
| 6 | Sequential crawling | Step 2.3: batch independent lookups; subagent-workflows |
| 7 | Context flooding: whole files/logs | Step 2.2: read narrowly |
| 8 | Analysis paralysis | Step 2.4: two rounds, then reason or stop |
| 9 | Plowing through contradictory evidence | Step 2.6: state surprise, reroute |
| 10 | Option-dump reports | Step 3: one recommendation; alternatives one line each |
| 11 | Scope creep: unsolicited edits | Steps 3, 4.3: scope, smallest change; simplify |
| 12 | Silent step-dropping | Step 4.4: written checklist, audit against ask |
| 13 | Retry thrash | Step 5: at most 3 attempts; hand back output/hypothesis |
| 14 | Verification theater: no run/broken build | Step 5(a,b): observed target + surrounding checks |
| 15 | Unauthorized outward action | Step 3: authorization → `AUTH:` |
| 16 | Silently dropped prescribed follow-up | Step 6: decision → `PENDING:` |
| 17 | Missed twins: one fix, copies remain | Step 5(c): pattern + search → `ISSUE:` |
| 18 | Costume rigor: thoroughness without evidence | Steps 5(c), 2.7: rerun searches, validate hits, label guesses |
| 19 | Scanner laundering: raw hits called defects | Step 2.7: open each hit; exclude placeholders/vendor/examples |

## Reading an audit

**Skipped** creates the listed risk; **faked** claims a step without observation (verification theater).
With time for three checks, prioritize 1, 13, 14. Weak-tier reporting failures 16/18 resisted prose changes;
action-point `TASK:`/`ISSUE:` lines transferred (`eval.md`).
