Reconstructed notes on the stock built-in /simplify skill; reference only, not instructions.
<!-- markdownlint-disable MD013 MD040 -->

# Built-in /simplify

A bundled skill (compiled into the Claude Code binary, source not public): `/simplify` or `/simplify focus on [area]`
runs an automated multi-agent review and cleanup of recently changed files, after a feature or fix and before a PR.
Reconstructed from public docs, community sources and behavioral analysis.

## Workflow

1. **Change detection**: `git diff` (or `git diff HEAD` for staged changes); outside git, the most recently
   modified files. The diff is the review scope.
2. **Three parallel sub-agents**, each given the full diff:
   - **Code Reuse**: duplicated logic to extract, existing helpers that should replace new code, repeated patterns.
   - **Code Quality**: naming, decomposition, control flow, `CLAUDE.md` compliance, smells (leaky abstractions,
     stringly-typed code, nesting), over-engineering and premature optimization.
   - **Efficiency**: needless allocations and recomputation, batchable loops, N+1 queries, inefficient file or
     network access, missed concurrency, unnecessary frontend re-renders.
3. **Aggregation**: findings deduplicated, false positives skipped, fixes applied directly, summary reported.

## Characteristics

| Property | Detail |
|----------|--------|
| **Agent count** | 3 parallel sub-agents |
| **Scope** | Recent changes only (`git diff`) |
| **Fix mode** | Automatic, with summary |
| **False positives** | Skipped rather than argued |
| **Customization** | Focus areas as arguments (`focus on performance`) |
| **Dependency** | Reads `CLAUDE.md` for project standards; well-defined ones give better results |

Recommended flow: build feature → run tests → `/simplify` → review diffs → commit. A post-implementation pass that
tightens verbose AI-generated code; not a replacement for human review.

Related: `/init` (creates the `CLAUDE.md` it reads), `/diff` (preview first), `/batch` (large-scale refactors across a
codebase, vs. `/simplify`'s recent changes).
