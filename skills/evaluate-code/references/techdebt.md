# Technical Debt Scan and Catalog

Find and catalog technical debt in a codebase (a module path, or the whole repo).

## Scan

Resolve the scope (a module path, or the repo), then gather context with the Bash tool:

```bash
git diff --name-only HEAD~10 -- <scope> 2>/dev/null || git ls-files <scope> | head -50
git grep -n -E 'TODO|FIXME|HACK|XXX|NOQA' -- <scope> | head -30
```

1. Read the files with the highest marker concentration.
2. Score each item 1-5 on Impact, Risk, and Effort, then rank by
   `Priority = (Impact + Risk) × (6 − Effort)` (max 50).
3. For the top 3 items, suggest a concrete fix.

Output a markdown table, highest score first:
`| Score | Category | File(s) | Description | Impact | Risk | Effort |`

## Fix

Top-down: take the highest-`Priority` item, apply its fix, re-run the `git grep` to confirm the
marker is gone, then move to the next. Stop and report if a fix needs a design decision.

Adapted from anthropics/knowledge-work-plugins (engineering), Apache-2.0.
