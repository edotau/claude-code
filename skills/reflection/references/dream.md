# `/reflection:dream` — Judgment Residue: Merge Near-Duplicates

> The daily background `claude-code memory dream` runs this on the Copilot CLI (claude-opus-5.5,
> Claude fallback); a manual run overrides it. `--dry-run` runs the same audit and reports what
> would change without editing.

`claude-code memory dream` does the deterministic hygiene (rotate, archive, exact dedupe, cap
enforcement). What it cannot do is judge **meaning** — two entries that say the same thing in
different words, a stale fact a newer one contradicts, a pattern that recurs across 3+ memories
and is a hard prohibition. That's this reference.

## Phase 1: Baseline

No file backup: `~/.claude/memory/` is a local git repo, so `git -C ~/.claude/memory diff` / `log`
is the undo path.

## Phase 2: Audit all memory layers

Read every memory file and build an inventory: bank files (lines, last updated), each touched
project's `MEMORY.md` + topic files (lines, duplicates, staleness).

## Phase 3: Review recent sessions

Run `claude-code memory timeline` for the recent harvest/dream/update trail:

1. Extract recurring themes (patterns appearing in 2+ sessions)
2. Extract corrections (things that failed and their fixes)
3. Note which are already captured in memory vs. missing

## Phase 4: Consolidate native project memory

For each `~/.claude/memory/<repo>/MEMORY.md` (index) + its topic files:

1. **Remove duplicates**: Entries/topic files that say the same thing in different words
2. **Remove contradictions**: If older entry is contradicted by newer, keep only newer
3. **Remove stale entries**: Items referencing files, APIs, or patterns that no longer exist
4. **Merge related items**: Fold overlapping topic files together; keep TOC entries coherent
5. **Keep `MEMORY.md` a lean index**: only its first ~200 lines auto-load, so detail lives in topic files — trim the
   index to one-line TOC entries, prioritizing:
   - Keep: Gotchas, active conventions, recent patterns
   - Remove: Generic observations, one-time context, resolved issues
6. **Never edit the RULES region** (`<!-- RULES:BEGIN -->`…`<!-- RULES:END -->`) without explicit approval

## Phase 5: Refresh the bank

Run `claude-code memory review` for cap/staleness findings, then:

1. **Current Focus** in `activeContext.md`: verify it reflects the latest session's state
2. **Next Steps**: remove completed items, keep active ones
3. **`sessionHistory.md`**: if over 30 entries, `claude-code memory index` rotates overflow into `archive/`

## Phase 6: Cross-pollinate (via subagent)

Hand the dream consolidator (opencode) every touched `~/.claude/memory/<repo>/MEMORY.md` (+
topic files). Ask it to:

1. Detect rules that recur across **3+ project memories** and are hard prohibitions.
2. Only those rules qualify for promotion to `CLAUDE.md` under "Known Gotchas" (or the
   equivalent always-on section) — and only if visible every session is genuinely required.
3. After promotion, remove the promoted entry from the individual project memories
   (project-level supersedes).
4. Rules that recur but aren't hard prohibitions get appended straight to the matching
   `rules/standards/` or `rules/software/<lang>/` file instead (via `claude-code memory promote`).

## Phase 7: Report

```text
Reflection & Dream Consolidation Report
════════════════════════════════════════

Native Project Memory:
  MEMORY.md  89 -> 71 lines  (-18 merged related items)
  [unchanged]    other topic files

Bank:
  activeContext.md: [updated/unchanged]
  sessionHistory.md: N entries (archived: N)

Cross-Pollination:
  1 pattern promoted to CLAUDE.md: "<rule>"
  Removed from: native project memory

Total changes: N files modified, N files unchanged
```

## Dry-run mode

When invoked with `--dry-run`:

1. Run all audit and analysis steps
2. Do NOT edit any files
3. Output what *would* change
4. End with: "Run `/reflection:dream` to apply these changes."

## Background run

> The daily, unattended run (no human watching the edits) — the same phases, with a tighter leash.

- **Edit only the memory root** (`~/.claude/memory/`). Never touch `CLAUDE.md` or `rules/**`
  unattended — list promotion candidates in the report instead of writing them.
- **Keep every `<!-- id:... -->` marker.** Archiving or rewriting an entry must carry its marker
  forward; a dropped id breaks every cross-reference to it.
- **Archive superseded decisions, never delete them** — strike the title line, point at the
  replacement, move the body to `archive/` only once the active file is over cap.
- **Respect the caps** (`rules/workflow/memory-bank.md`): projectContext 60, activeContext 80,
  progress 60, conventions 80, decisionLog 200, sessionHistory 30 entries.
- **The RULES region is untouchable** — `<!-- RULES:BEGIN -->`…`<!-- RULES:END -->` in any native
  `MEMORY.md`, unattended or not.

## Verification

After all updates:

1. Confirm all updated memory files are well-formed markdown
2. Confirm `sessionHistory.md` has <= 30 entries
3. Confirm the project `MEMORY.md` is a lean index (detail pushed to topic files; RULES region untouched)

## When to run

- **Periodically**: for memory hygiene (daily background `memory dream`)
- **Proactively**: when context window is ≥85% full (the `context-checkpoint` Stop hook's
  threshold, before compaction loses detail)
- **After corrections**: when user feedback or gate failures revealed a pattern worth capturing
  — route those into `rules/` via `/reflection:promote --to rules` first; dream only
  cross-pollinates what has already settled into 3+ memories

## Integration

- `claude-code memory timeline` — harvest/dream/update receipts + sessionHistory, the real recent-session trail
