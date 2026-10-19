---
description: "Session reflection + memory consolidation — persist learnings, then clean stale entries, merge insights, prune low-signal items"
---

# /reflection:consolidate — Session Reflection & Memory Consolidation

> Part of the `/reflection:*` family (see `/reflection:review`, `:promote`, `:extract`,
> `:remember`). This command owns the end-of-session reflection + dream consolidation
> pipeline; the rest of the family curates Claude Code's native auto-memory.

Two modes in one command:

- **`/reflection:consolidate`** — End-of-session: summarize work, persist learnings to native project memory and the memory bank, then consolidate
- **`/reflection:consolidate --dream`** — Consolidation only: audit, deduplicate, prune, and cross-pollinate memory without a session summary
- **`/reflection:consolidate --dry-run`** — Audit all memory layers and report what would change without editing

Run before ending a session, when context pressure is high (>80% window), or periodically to keep memory clean.

---

## Part 1: Session Reflection

> Skip this part when invoked with `--dream` or `--dry-run`.

### Step 1: Session Summary

Review the conversation so far. Produce a structured summary:

```markdown
# Session: YYYY-MM-DD
**Branch**: <current branch>

## Tasks Completed
- <brief description of each completed task>

## Files Modified
- <list of key files changed this session>

## Decisions Made
- <architectural or design decisions and their rationale>

## Issues Encountered
- <problems hit and how they were resolved, or still open>
```

### Step 2: Update Native Project Memory

Update the current project's native memory in its store dir (`claude-code memory path`, under
`~/.claude/memory/<repo>/`), the same dir the typed bank files live in:

1. Resolve the project's memory dir: `$(claude-code memory path)/MEMORY.md`
2. Read its `MEMORY.md` index (+ any linked topic files)
3. Summarize new learnings into a **topic file** (`<topic>.md` beside `MEMORY.md`), then add or refine its one-line TOC entry in `MEMORY.md`:
   - Patterns that recur in this project
   - Gotchas discovered this session
   - Conventions to enforce
   - Things that worked well or failed
4. Only write what was actually learned this session — no generic platitudes
5. Never edit the approval-gated RULES region (`<!-- RULES:BEGIN -->`…`<!-- RULES:END -->`)

### Step 3: Update the Memory Bank

Run `claude-code memory path` to resolve the bank for this repo, then update:

- **`activeContext.md`** (contracts: `rules/workflow/memory-bank.md`) — Current Focus, Next
  Steps. (The dated session summary goes to `sessionHistory.md` — `/memory:end` owns it;
  append there only if `/memory:end` has not run this session.)
- **`progress.md`** (if changed) — What Works / What's Next / Known Issues.

### Step 4: Route Corrections to Rules (if any occurred)

If this session revealed a mistake, wrong pattern, or missing constraint:

1. Identify the root cause.
2. Route it straight into the matching `rules/standards/<topic>.md` or
   `rules/software/<lang>/<topic>.md` section as a concise, imperative rule (the same
   routing table as `/harness:adapt`). If a similar rule already exists, strengthen it
   instead of duplicating.
3. **Do NOT write to CLAUDE.md from this step.** Promotion to CLAUDE.md happens only in
   Phase 7 below under strict criteria.

---

## Part 2: Dream Consolidation

> Runs automatically after Part 1, or standalone with `--dream` / `--dry-run`.

Launch the **opencode** agent (PERSIST mode) for consolidation. Pass it all memory file contents as context.

### Phase 1: Baseline

No file backup: `~/.claude/memory/` is a local git repo, so `git -C ~/.claude/memory diff` / `log` is the undo path.

### Phase 2: Audit All Memory Layers

Read every memory file and build an inventory: bank files (lines, last updated), each
touched project's `MEMORY.md` + topic files (lines, duplicates, staleness).

### Phase 3: Review Recent Sessions

Run `claude-code memory timeline` for the recent harvest/dream/session trail:

1. Extract recurring themes (patterns appearing in 2+ sessions)
2. Extract corrections (things that failed and their fixes)
3. Note which are already captured in memory vs. missing

### Phase 4: Consolidate Native Project Memory

For each `~/.claude/memory/<repo>/MEMORY.md` (index) + its topic files:

1. **Remove duplicates**: Entries/topic files that say the same thing in different words
2. **Remove contradictions**: If older entry is contradicted by newer, keep only newer
3. **Remove stale entries**: Items referencing files, APIs, or patterns that no longer exist
4. **Merge related items**: Fold overlapping topic files together; keep TOC entries coherent
5. **Keep `MEMORY.md` a lean index**: only its first ~200 lines auto-load, so detail lives in topic files — trim the index to one-line TOC entries, prioritizing:
   - Keep: Gotchas, active conventions, recent patterns
   - Remove: Generic observations, one-time context, resolved issues
6. **Never edit the RULES region** (`<!-- RULES:BEGIN -->`…`<!-- RULES:END -->`) without explicit approval

### Phase 5: Refresh the Bank

Run `claude-code memory doctor` for cap/staleness findings, then:

1. **Current Focus** in `activeContext.md`: verify it reflects the latest session's state
2. **Next Steps**: remove completed items, keep active ones
3. **`sessionHistory.md`**: if over 30 entries, `claude-code memory index` rotates overflow into `archive/`

### Phase 6: Cross-Pollinate (via subagent)

Hand the dream consolidator (opencode) every touched `~/.claude/memory/<repo>/MEMORY.md` (+
topic files). Ask it to:

1. Detect rules that recur across **3+ project memories** and are hard prohibitions.
2. Only those rules qualify for promotion to `CLAUDE.md` under "Known Gotchas" (or the
   equivalent always-on section) — and only if visible every session is genuinely required.
3. After promotion, remove the promoted entry from the individual project memories
   (project-level supersedes).
4. Rules that recur but aren't hard prohibitions get appended straight to the matching
   `rules/standards/` or `rules/software/<lang>/` file instead.

### Phase 7: Report

Output a structured summary:

```
Reflection & Consolidation Report
══════════════════════════════════

Session Summary: [written / skipped (dream-only)]

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

---

## Dry-Run Mode

When invoked with `--dry-run`:

1. Run all audit and analysis steps
2. Do NOT edit any files
3. Output what *would* change
4. End with: "Run `/reflection:consolidate` to apply these changes."

---

## Verification

After all updates:

1. Confirm all updated memory files are well-formed markdown
2. Confirm `sessionHistory.md` has <= 30 entries
3. Confirm the project `MEMORY.md` is a lean index (detail pushed to topic files; RULES region untouched)

---

## When to Run

- **Always**: Before ending a session (the Stop hook reminds you)
- **Proactively**: When context window is >80% full (before compaction loses detail)
- **After corrections**: When user feedback or gate failures revealed a pattern worth capturing
- **Periodically**: For memory hygiene (`--dream` mode)

---

## Integration

- `claude-code memory timeline` — harvest/dream/reflect receipts + sessionHistory, the real recent-session trail
