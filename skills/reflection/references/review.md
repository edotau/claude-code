# `/reflection:review` (no flag) — Judgment Residue: Ranking Promotion Candidates

> Read-only. `claude-code memory review` computes caps, staleness, exact dupes, and hot/cold
> signals from `access.log` deterministically. `--brief` and `--stale-days N` need no further
> judgment — report the CLI output directly. the unflagged default needs a
> model to rank entries against the promotion gradient; that's this reference. Never modify,
> promote, or create files in this mode — that's `/reflection:promote`'s job.

## Memory layer inventory

Read in order:

1. **Catalog** — `memory/index.md` (the generated router, one row per repo store); `claude-code
   memory path` resolves this repo's store dir (`memory/<repo>/`, harness = `memory/claude-code/`)
2. **Native `MEMORY.md` + topic files** — in that same store dir (first ~200 lines of `MEMORY.md`
   auto-load, including its generated `<!-- bank:begin -->`…`<!-- bank:end -->` region)
3. **Typed bank files** — the six files beside `MEMORY.md` in the same dir; `claude-code memory
   review` surfaces caps/staleness/cross-tier-duplication across both native and typed
4. **Rules tiers** — `rules/standards/*.md`, `rules/workflow/*.md`
5. **Always-on tier** — the global `~/.claude/CLAUDE.md` and the project `./CLAUDE.md`

Use `Glob`/`Read`/`Grep` to inventory and cross-reference, `Bash` for `claude-code memory review`
and `memory archive --dupes` (audit mode — never pass `--apply` here).

## Analysis process

1. **Inventory.** Count lines per layer; note whether `MEMORY.md` nears the 200-line cap (flag if > 180).
2. **Cross-reference against harness tiers.** Duplicate of CLAUDE.md/rules? Conflicts with one
   (show both sides)? Or fills a gap no tier captures (promotion candidate)?
3. **Detect staleness.** Dead paths (`ls`/`find`), dead function/flag names (`Grep` the
   codebase), facts a rule now overrides. `memory review` feeds this directly.
4. **Detect recurrence.** Paraphrased duplicates; "again"/"still"/"always" language. Group
   consolidation candidates by topic.
5. **Score every entry** on Durability, Impact, Scope (0–3 each). Promotion candidate: total ≥ 6
   AND not already in a higher tier.

## Output format

```markdown
## Memory Health Report — <date>

### Inventory
- MEMORY.md: N entries, N lines (cap: 200)
- Topic files: <list with line counts>
- memory review findings: <summary>

### Promotion Candidates  (score ≥ 6, sorted descending)
| Score | Entry | Current tier | Recommended tier (`--to home\|rules\|skill`) | Gate status |

### Stale Entries
| Entry | Staleness reason | Verify command |

### Consolidation Groups
- Group A: [entry1], [entry2] — same concept

### Conflicts
| Entry | Conflicts with | Both sides |

### Health Metrics
- Capacity: N/200 lines (N%)
- Entries with dead path refs / duplicating higher tiers: N

### Recommendations (top 3)
```

Be specific — line numbers, exact entry text (or first 80 chars), concrete next steps. The report
should be shorter than the memory it analyzes.

## What you don't do

- Don't promote entries or edit any file (that's `/reflection:promote` or the human); don't write
  to `CLAUDE.md` (that gate is `/reflection:dream` — you surface candidates); don't invent entries;
  no destructive bash (`memory review` only, never `--apply`).

## Failure Modes

- **`MEMORY.md` empty/missing** — report as a gap; offer the expected format as a scaffold.
