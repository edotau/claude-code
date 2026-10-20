---
description: "Find and catalog technical debt in this codebase"
argument-hint: "[scan | fix | module path]"
---

Find and catalog technical debt in this codebase.

Context — recently changed files:
```
!git diff --name-only HEAD~10 2>/dev/null || find . -name '*.py' -o -name '*.ts' -o -name '*.tsx' | head -50
```

Current TODOs and FIXMEs:
```
!grep -rn "TODO\|FIXME\|HACK\|XXX\|NOQA" --include='*.py' --include='*.ts' --include='*.tsx' . 2>/dev/null | head -30
```

Instructions:
1. Scan the recently changed files and TODO/FIXME markers above.
2. Read the files with the highest concentration of markers.
3. Categorize debt:

   | Category | Examples | Risk if left |
   |---|---|---|
   | **Code** | Duplicated logic, poor abstractions, magic numbers, dead code, stale comments | Bugs, slow development |
   | **Architecture** | Wrong abstraction level, tight coupling, wrong data store | Scaling limits |
   | **Test** | Missing coverage, brittle or flaky tests, skipped tests | Regressions ship |
   | **Performance** | N+1 queries, missing indexes, unoptimized loops | Latency, cost |
   | **Migration** | Incomplete migrations, multiple patterns for the same thing | Every change done twice |
   | **Dependency** | Outdated or unmaintained libraries | Security vulns |
   | **Documentation** | Missing runbooks, outdated READMEs, tribal knowledge | Onboarding pain |
   | **Infrastructure** | Manual deploys, no monitoring, no IaC | Incidents, slow recovery |

4. Score each item 1–5 on Impact (how much it slows the team), Risk (what happens if unfixed) and Effort
   (how hard the fix is), then rank by `Priority = (Impact + Risk) × (6 − Effort)` (max 50).
5. For the top 3 items, suggest a concrete fix.

Output a markdown table, highest score first:
| Score | Category | File(s) | Description | Impact | Risk | Effort |

Adapted from anthropics/knowledge-work-plugins (engineering), Apache-2.0.
