# Technical Debt Scan and Catalog

Find and catalog technical debt in a codebase (a module path, or the whole repo).

## Scan

Resolve the scope (a module path, or the repo), then gather context with the Bash tool:

```bash
git diff --name-only HEAD~10 -- <scope> 2>/dev/null || git ls-files <scope> | head -50
git grep -n -E 'TODO|FIXME|HACK|XXX|NOQA' -- <scope> | head -30
```

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

4. Score each item 1-5 on Impact (how much it slows the team), Risk (what happens if unfixed) and
   Effort (how hard the fix is), then rank by `Priority = (Impact + Risk) × (6 − Effort)` (max 50).
5. For the top 3 items, suggest a concrete fix.

Output a markdown table, highest score first:
`| Score | Category | File(s) | Description | Impact | Risk | Effort |`

## Fix

Work the catalog top-down: take the highest-`Priority` item, apply its suggested concrete fix,
re-run the scan's `git grep` for that scope to confirm the marker is gone, then move to the next
item. Stop and report if a fix needs a design decision the catalog can't make alone.

Adapted from anthropics/knowledge-work-plugins (engineering), Apache-2.0.
