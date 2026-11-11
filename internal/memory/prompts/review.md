# `memory review --candidates` — Judgment Residue: Ranking Promotion Candidates

> `claude-code memory review` prints today's caps/staleness stats first; this is what
> ranks them against the promotion gradient. Read-only — never modify, promote, or create
> files here; that's `memory promote`'s job.

## Memory layer inventory

Read in order:

1. **Catalog** — `memory/index.md` (the generated router, one row per repo store); `claude-code
   memory path` resolves this repo's store dir (`memory/<repo>/`, harness = `memory/claude-code/`)
2. **Native `MEMORY.md` + topic files** — in that same store dir (the native loader reads `MEMORY.md` up to
   200 lines / 25 KB, including its generated `<!-- bank:begin -->`…`<!-- bank:end -->` region of ≤ 40 lines)
3. **Typed bank files** — the six files beside `MEMORY.md` in the same dir; `claude-code memory
   review` surfaces caps/staleness across both native and typed
4. **Rules tiers** — `rules/standards/*.md`, `rules/workflows/*.md`
5. **Always-on tier** — the global `~/.claude/CLAUDE.md` and the project `./CLAUDE.md`

Use `Glob`/`Read`/`Grep` to inventory and cross-reference, `Bash` for `claude-code memory review`;
caps: `rules/workflows/memory-bank.md` → Caps.

## Analysis process

1. **Inventory.** Count lines per layer; note whether `MEMORY.md` nears the native index cap (200 lines / 25 KB; flag if > 180 lines).
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
- MEMORY.md: N entries, N lines (index cap: 200)
- Topic files: <list with line counts>
- memory review findings: <summary>

### Promotion Candidates  (score ≥ 6, sorted descending)
| Score | Entry | Current tier | Recommended tier (`--to home\|rules\|skill`; `claude-md` is dream-gated) | Gate status |

### Stale Entries
| Entry | Staleness reason | Verify command |

### Consolidation Groups
- Group A: [entry1], [entry2] — same concept

### Conflicts
| Entry | Conflicts with | Both sides |

### Health Metrics
- Capacity: N/200 lines (N%) of the native index; typed files against `rules/workflows/memory-bank.md` → Caps
- Entries with dead path refs / duplicating higher tiers: N

### Recommendations (top 3)
```

Be specific — line numbers, exact entry text (or first 80 chars), concrete next steps. The report
should be shorter than the memory it analyzes.

## What you don't do

- Don't promote entries or edit any file (that's `memory promote` or the human); don't write
  to `CLAUDE.md` (that gate is `memory dream` — you surface candidates); don't invent entries;
  no destructive bash (`memory review` only, never `--apply`).

## Failure Modes

- **`MEMORY.md` empty/missing** — report as a gap; offer the expected format as a scaffold.
