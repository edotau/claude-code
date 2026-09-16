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
3. Categorize debt as:
   - **Architecture**: Wrong abstraction level, missing service layer, tight coupling
   - **Testing**: Missing coverage, brittle tests, skipped tests
   - **Performance**: N+1 queries, missing indexes, unoptimized loops
   - **Cleanup**: Dead code, unused imports, stale comments
   - **Migration**: Incomplete migrations, multiple patterns for the same thing
4. Rank by impact (how much it slows down future work).
5. For the top 3 items, suggest a concrete fix with estimated effort (small/medium/large).

Output a markdown table:
| Priority | Category | File(s) | Description | Effort |
