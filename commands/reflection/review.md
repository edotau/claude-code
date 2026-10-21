---
description: "Normally automatic (dream consolidation) — manual audit of native project memory: health dashboard (--brief/--quick) or a full read-only audit of promotion candidates, stale entries, and consolidation opportunities via the opencode agent (ANALYZE mode)."
argument-hint: "[--brief | --quick | --stale | --candidates]"
---

# /reflection:review — Analyze Project Memory

> Part of the `/reflection:*` family (see `/reflection:promote`, `:extract`, `:remember`,
> `:consolidate`). This command runs a **read-only** audit of Claude Code's native project
> memory (and, when relevant, the session memory bank) and surfaces what to promote, prune,
> or merge. It changes nothing — acting on findings is `/reflection:promote` and
> `/reflection:consolidate`'s job.

## Usage

```text
/reflection:review             # Full review (dashboard + candidates + stale + consolidation)
/reflection:review --brief      # One-line health summary only (no agent dispatch)
/reflection:review --quick      # Health dashboard + top 3 promotion candidates
/reflection:review --stale      # Focus on stale / outdated entries
/reflection:review --candidates # Focus on promotion candidates only
```

## `--brief` / `--quick` (no dispatch)

Run directly, no subagent:

```bash
claude-code memory doctor
```

Report: bank cap usage (activeContext/progress/decisionLog/conventions/sessionHistory vs
their caps), any stale findings, and (for `--quick`) a quick scan of
`~/.claude/memory/*/MEMORY.md` line counts flagged if any are near the 200-line
auto-load cap.

## Full review (default, `--stale`, `--candidates`)

Dispatch the **opencode** agent in **ANALYZE** mode. Give it:

- Every `~/.claude/memory/*/MEMORY.md` + linked topic files
- The output of `claude-code memory doctor`
- `rules/standards/*.md`, `rules/workflow/*.md`, and this project's `CLAUDE.md`

It returns the Memory Health Report (inventory, promotion candidates scored 0-6 on
Durability+Impact+Scope, stale entries, consolidation groups, conflicts). Filter the report
to the requested focus (`--stale` → Stale Entries section only; `--candidates` →
Promotion Candidates only); default shows everything.

## Report

Present the agent's report verbatim, followed by a one-line next-step suggestion per
finding category (e.g. "3 candidates ≥ score 6 → run `/reflection:promote` on each").

## Procedure

A read-only analyst. Assess the health of the memory stack, score every entry against the
harness promotion gradient, and produce a concise, prioritized report. Never modify, promote, or
create files in this mode.

### Memory layer inventory

Read in order:

1. **Catalog** — `memory/index.md` (the generated router, one row per repo store); `claude-code
   memory path` resolves this repo's store dir (`memory/<repo>/`, harness = `memory/claude-code/`)
2. **Native MEMORY.md + topic files** — in that same store dir (first ~200 lines of `MEMORY.md`
   auto-load, including its generated `<!-- bank:begin -->`…`<!-- bank:end -->` region)
3. **Typed bank files** — the six files beside MEMORY.md in the same dir; `claude-code memory
   doctor` surfaces caps/staleness/cross-tier-duplication across both native and typed
4. **Rules tiers** — `rules/standards/*.md`, `rules/workflow/*.md`
5. **Always-on tier** — the global `~/.claude/CLAUDE.md` and the project `./CLAUDE.md`

Use `Glob`/`Read`/`Grep` to inventory and cross-reference, `Bash` for `claude-code memory doctor`
and `memory archive --dupes` (audit mode — never pass `--apply` in ANALYZE).

### Analysis process

1. **Inventory.** Count lines per layer; note whether MEMORY.md nears the 200-line cap (flag if > 180).
2. **Cross-reference against harness tiers.** Duplicate of CLAUDE.md/rules? Conflicts with one
   (show both sides)? Or fills a gap no tier captures (promotion candidate)?
3. **Detect staleness.** Dead paths (`ls`/`find`), dead function/flag names (`Grep` the
   codebase), facts a rule now overrides. `memory doctor` feeds this directly.
4. **Detect recurrence.** Paraphrased duplicates; "again"/"still"/"always" language. Group
   consolidation candidates by topic.
5. **Score every entry** on Durability, Impact, Scope (0–3 each). Promotion candidate: total ≥ 6
   AND not already in a higher tier.

### ANALYZE output format

```markdown
## Memory Health Report — <date>

### Inventory
- MEMORY.md: N entries, N lines (cap: 200)
- Topic files: <list with line counts>
- memory doctor findings: <summary>

### Promotion Candidates  (score ≥ 6, sorted descending)
| Score | Entry | Current tier | Recommended tier | Gate status |

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

Be specific — line numbers, exact entry text (or first 80 chars), concrete next steps. The
report should be shorter than the memory it analyzes.

### What you don't do

- **ANALYZE**: don't promote entries or edit any file (that's `/reflection:promote` or the
  human); don't write to CLAUDE.md (that gate is `/reflection:consolidate --dream` — you surface
  candidates); don't invent entries; no destructive bash (`memory doctor` only, never `--apply`).

### Failure Modes

- **MEMORY.md empty/missing (ANALYZE)** — report as a gap; offer the expected format as a scaffold.
