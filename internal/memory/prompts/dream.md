# `claude-code memory dream` — Judgment Residue: Merge Near-Duplicates

What Go cannot judge is **meaning** — two entries that say the same thing in different words, a
stale fact a newer one contradicts, a pattern that recurs across 3+ memories and is a hard
prohibition. That's this reference. (`memory dream --dry-run` is the separate, deterministic
preview — no model call, see its own output.)

Tools: Read, Grep and Glob to inspect; Edit and Write under the memory root (allowed, despite the gate's
`mode: plan`); Bash only for the `claude-code memory` commands this file names, exactly as written (no `2>&1`,
redirects, pipes, `cd`, `wc` or `git`). A refused call is not a reason to stop: continue with the allowed tools.

## Phase 1: Baseline

No file backup: `~/.claude/memory/` is a local git repo, so `git -C ~/.claude/memory diff` / `log`
is the undo path.

## Phase 2: Audit all memory layers

Read only the files in the prompt's "Changed since the last dream" list, plus any file a phase needs to judge
them (the over-cap file being archived, the entry a change supersedes). The list also covers `feedback/` and
`notes/`. Never read the whole bank; an empty list means skip to Phase 5.

## Phase 3: Review recent sessions

Run `claude-code memory review --timeline` for the recent update/dream trail:

1. Extract recurring themes (patterns appearing in 2+ sessions)
2. Extract corrections (things that failed and their fixes)
3. Note which are already captured in memory vs. missing

## Phase 4: Consolidate native project memory

For the shared `~/.claude/memory/MEMORY.md` (index) and the topic files beside it (`<topic>.md`, in their folders):

1. **Remove duplicates**: Entries/topic files that say the same thing in different words
2. **Remove contradictions**: If older entry is contradicted by newer, keep only newer
3. **Remove stale entries**: Items referencing files, APIs, or patterns that no longer exist
4. **Merge related items**: Fold overlapping topic files together; keep TOC entries coherent
5. **Do not hand-prune `MEMORY.md`**: it is generated (pinned notes plus the newest 20 pointers). Pointer lines you
   add are appended to `index.md`, which Go never rewrites; to keep a note in view set `metadata.pinned: true`
   When rewriting a note, keep its existing `metadata` keys, including `pinned`.
6. **Never edit the RULES region** (`<!-- RULES:BEGIN -->`…`<!-- RULES:END -->`) without explicit approval

## Phase 5: Refresh the bank

Run `claude-code memory review` for cap/staleness findings, then:

1. **Current Focus** in `activeContext.md`: verify it reflects the latest session's state
2. **Next Steps**: remove completed items, keep active ones
3. **`sessionHistory.md`**: if over 30 entries, `claude-code memory index` rotates overflow into `archive/`

## Phase 5b: Feedback candidates

Propose, never apply: this phase edits no code, no rule and no plan, only `knowledge/feedback-candidates.md`.
A lesson that recurs across `feedback/` and `notes/` notes may belong in a tool instead of prose. Using the changed
notes, Grep to find other notes that restate the same lesson, then rewrite that one file (a prose header, then one
fenced block). Each line of the block is one JSON object with exactly these fields, no others:

```jsonl
{"id":"fc-<slug>","evidence":["feedback/<a>.md","notes/<b>.md"],"repeats":0,"change":"One sentence.","target":"rules/standards/<x>.md","kind":"hook","size":"small"}
```

- `id`: `fc-` plus lower-case letters, digits and hyphens; unique and never renumbered.
- `evidence`: at least two distinct note paths that exist in the store, or one path plus `repeats` of at least 2
  (how often the owner restated it); one remark is never a candidate.
- `change`: one sentence, at most 200 characters. `target`: a path inside `~/.claude` or the store, or `unknown`.
- `kind`: `prose`, `hook`, `verb` or `test`. `size`: `small`, `medium` or `large` (an estimate).
- Never write a credential-shaped string. Run `claude-code memory review` after writing: it prints one line per
  problem with its line number and exits non-zero; fix them before Phase 6.

## Phase 5c: Test proposals

Collect the tests agents proposed but did not write ("Proposed tests", "proposals" in `reports/`, `handoffs/` and
`sessionHistory.md`) into `techdebt/tests.md`: one line each (package, case, what it guards, source report), newest
first, at most 40 lines. Drop a line once a test covers that behavior or the feature is gone. The owner picks from it.

## Phase 6: Cross-pollinate

Using the changed `MEMORY.md` sections and topic files already read (no subagent, no re-read):

1. Detect rules that recur across **3+ project memories** and are hard prohibitions.
2. Only those rules qualify for promotion to `CLAUDE.md` → Hard rules (always-on tier) — and only if visible
   every session is genuinely required. Run `claude-code memory promote <id> --to claude-md`: it works only in this
   run, prints the block and edits nothing, so you apply the edit by hand.
3. After promotion, remove the promoted entry from the individual project memories
   (project-level supersedes).
4. Rules that recur but aren't hard prohibitions go in the report as `rules/standards/` or
   `rules/software/<lang>/` candidates (`claude-code memory promote <id> --to rules` prints the target); this run
   never edits `rules/**`.

Never move entries to the home bank: list candidates in the report (`claude-code memory review
--candidates`); only the owner runs `memory promote <id> --to home`.

## Phase 7: Report

End with a short plain summary: files changed, promotion candidates, anything skipped.

## Background run

> The daily, unattended run (no human watching the edits) — the same phases, with a tighter leash.

- **Edit only the memory root** (`~/.claude/memory/`), plus `CLAUDE.md` for a block the Phase 6 gate printed. Never
  touch `rules/**` unattended — list promotion candidates in the report instead of writing them.
- **Keep every `<!-- id:... -->` marker.** Archiving or rewriting an entry must carry its marker
  forward; a dropped id breaks every cross-reference to it.
- **Archive superseded decisions, never delete them** — write `Supersedes <old id>` in the
  replacing entry (the harness strikes the old one), move the body to `archive/` only once the active file is over cap.
- **Respect the caps** — `rules/workflows/memory-bank.md`; Go enforces them on settle, but a
  judgment edit must not write past one either.
- **The RULES region is untouchable** — `<!-- RULES:BEGIN -->`…`<!-- RULES:END -->` in any native
  `MEMORY.md`, unattended or not.

## Verification

After all updates:

1. Confirm all updated memory files are well-formed markdown
2. Confirm `sessionHistory.md` has <= 30 entries and each typed file is within its cap
3. Confirm the topic files are coherent (RULES region untouched); `MEMORY.md` is generated, never edited by hand

## When to run

- **Periodically**: for memory hygiene (daily background `memory dream`)
- **Proactively**: when context window is ≥85% full (the `context-checkpoint` Stop hook's
  threshold, before compaction loses detail)
- **After corrections**: when user feedback or gate failures revealed a pattern worth capturing
  — route those into `rules/` via `memory remember` + `memory promote <id> --to rules` first; dream only
  cross-pollinates what has already settled into 3+ memories

## Integration

- `claude-code memory review --timeline` — update/dream receipts + sessionHistory, the real recent-session trail
