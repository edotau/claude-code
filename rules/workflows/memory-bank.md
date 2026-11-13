---
paths:
  - "internal/memory/**"
---
# Memory Bank — Session-to-Session Store (`~/.claude/memory/<repo>/`)

`~/.claude/memory/` is one local-only git repo: `index.md` is the generated catalog, `memory/<repo>/` a repo's store
(the harness is `memory/claude-code/`, non-repo cwds use `memory/home/`). Each store holds Claude Code's native
auto-memory (`MEMORY.md` + topics) **and** six typed bank files — one directory, not siblings:
`~/.claude/projects/<slug>/memory` is a symlink into it, repaired at SessionStart. The bank holds session STATE;
durable facts belong in `rules/` or `CLAUDE.md` — point at them, never copy.

Scratch and fixture folders (`$TMPDIR`, `/tmp`, `~/.claude/state`, `~/worktrees/wf-fixtures`) never get a store of
their own, git repo or not: they use the launching workspace's store, which every harness launch passes as
`HARNESS_MEMORY_ORIGIN`, else `memory/home/` (`throwawayRoots` in `internal/memory`).

## Precedence

The code under `internal/memory` is the source of truth, then this file (store layout, file contracts, caps, verbs),
then the prompts embedded in `internal/memory/prompts/` (`claude-code memory <verb>` prints them). Each fact lives in
one of these; the rest point to it, and on a disagreement the earlier one wins.

## Verbs

`claude-code memory -h` lists the menu verbs; the plumbing verbs are dispatchable but off that menu.
`memory <verb> -h` lists a verb's flags.

| Verb | Does |
| --- | --- |
| `recall <terms>` | Rank bank blocks for terms; `--load` appends the top bodies |
| `remember <fact>` | Append a fact to the bank; `--type` picks the file |
| `init` | Create the repo's bank |
| `review` | Doctor + bank stats; `--candidates` prints the promotion-ranking procedure |
| `promote <id> --to <target>` | Move or surface one block (`home\|rules\|skill\|claude-md`) |
| `update` | Save the live session or `--transcript`; with neither, print the procedure |
| `dream` | Consolidate the bank when due; `--force`, `--dry-run` |
| `search [--repo R \| --all] <terms>` | Rank blocks for terms, with scores |
| `show <id>` | Print one block (id from `search` or a `MEMORY.md` region) |
| `index` | Settle the bank: rotate and archive over caps; refresh the embedding cache |
| `path` | Print the bank directory |
| `report` | Append a report and its decisions to the bank's `reports/` |
| `digest` | Write the bank digest |
| `fmt` | Wrap the bank files; `--check` reports only |
| `watch`, `catchup` | The idle watcher and the missed-session sweep that the hooks spawn |
| `timeline` | Recent bank entries and sessionHistory |
| `alias --repo <checkout> --into <key>` | Fold a second checkout's store into another |
| `import --from <dir>\|--host <h>` | Merge another machine's or a backup's bank into this one |

`import` is additive and idempotent (`--dry-run` previews; `git -C ~/.claude/memory log` is the undo). By default it
takes logs only: `decisionLog.md`, `conventions.md`, `sessionHistory.md` and the monthly archives **merge** (missing
blocks only). `--topics` adds everything else: `MEMORY.md` pointer lines merge, files only the source has are
**copied**, and `activeContext.md`, `progress.md`, `projectContext.md` are **kept local**, as is any other differing
file (reported as a conflict); it cannot tell a new file from one this machine deleted on purpose. Never imported:
the catalog, lock files, `.git/`, symlinks, files over 4 MiB, and stores with no local counterpart.

A verb needing judgment prints its embedded procedure for the live session (or the headless `memory update`) to
apply; all others print none:

| Verb | Prints a procedure when |
| --- | --- |
| `init` | `projectContext.md` is still the template |
| `review` | `--candidates` (the ranking procedure) |
| `promote <id> --to <target>` | `--to rules\|skill`, and `--to claude-md` inside `memory dream` |
| `update` | no session or `--transcript` resolves (live session) |
| `dream` | the headless run hands it to the model; `--dry-run` is a deterministic preview, no model |

## Promotion ladder

The one copy; the other docs point here. Each step up costs more context:

1. `promote --to home` — moves the block (no link back) to the home bank (`~/.claude/memory/home/`, cross-project).
2. `promote --to rules` — prints the block for `rules/standards/<topic>.md` or `rules/software/<lang>/<topic>.md`
   (enforced); never `rules/workflows/` or `CLAUDE.md`. `promote --to skill` — prints it for
   `skills/<name>/SKILL.md` or its `references/` (how-to).
3. `promote --to claude-md` — `CLAUDE.md` → Hard rules (always-on tier). Gated: 3+ bank entries and a hard
   prohibition, so the verb runs only inside `memory dream`'s cross-pollination step and is refused anywhere else.

Only `home` writes; every other target prints the block, its target and the procedure, and the agent edits by hand.

## Files + mutation contracts

| File | Holds | Contract |
| --- | --- | --- |
| `projectContext.md` | Orientation + do-not-regress invariants | Replace, sparingly |
| `activeContext.md` | Focus, open questions, blockers, next steps, parked | **Replace** each session end |
| `progress.md` | What Works / In Progress / What's Next / Known Issues | **Replace** each session end |
| `decisionLog.md` | Dated decisions + rationale | **Append-only, newest first; supersede (strike + pointer), never delete** |
| `conventions.md` | Learned patterns not yet promoted | Append; `memory dream` prunes |
| `sessionHistory.md` | Newest-first summaries | Append at top; past the entry cap rotates to `archive/sessionHistory-<yyyy-mm>.md` |

Typed files carry `harness_kind: bank` frontmatter. `MEMORY.md` carries a generated `<!-- bank:begin -->`…
`<!-- bank:end -->` region; lines outside it are Claude's own and never rewritten.

## Caps

| Limit | Value | Defined by |
| --- | --- | --- |
| `projectContext.md` · `progress.md` | 60 lines each | `fileCaps` |
| `activeContext.md` · `conventions.md` | 80 lines each | `fileCaps` |
| `decisionLog.md` | 200 lines | `fileCaps` |
| `sessionHistory.md` | 30 entries (rotates, not a line cap) | `sessionHistoryMaxEntries` |
| Generated bank region of `MEMORY.md` | 20 lines / 2 KB, markers included | `bankRegionMaxLines`, `bankRegionMaxBytes` |
| Whole `MEMORY.md` (native index) | 200 lines / 25 KB | Claude Code native loader limits |

The 20-line region and the 200-line index are different things: the region is the harness's slice, the index is
everything the native loader reads. Past a `fileCaps` limit `memory dream` archives the overflow.

## Who writes what

- `memory-start` (SessionStart) — injects a pointer to the catalog + this repo's Current Focus/Blockers/Next Steps and
  In Progress; the rest loads on demand. It also scaffolds a missing bank, spawns the daily `memory dream` when
  due, and spawns one detached sweep (`memory catchup`) that saves up to 3 (`catchupMax`) missed interactive sessions
  from the last 14 days (`catchupWindow`), skips live ones, and prints one quiet line at the next start. Replays run
  `memory update --historical`: history and decision log only, never current focus.
- `memory-recall` (UserPromptSubmit) — injects a few matching blocks; another repo's store only when the prompt names
  it.
- `claude-code memory update` — the session-close writer: summary → sessionHistory, rewrite activeContext + progress,
  decisions → decisionLog, patterns → conventions.
- `memory-update` / `memory-end` (Stop / SessionEnd) — after enough edits, run `memory update` headless so a session
  that forgets still persists; the first dispatch claims a detached `memory watch` that updates after 10 quiet
  minutes (a crash fires no SessionEnd). `memory-compact` does the same before compaction. A detached worker
  (`CLAUDE_MEMORY_DETACHED`) queues up to `detachedLockWait` (16 min) for the store lock behind a dream; a foreground
  `memory update` waits `updateLockWait` (5 s) and says so.
  When the permission gate refused writes and no bank file changed, the update runs once more; a second miss records
  status `blocked` (no retry, no Stop block) and shows one quiet line: dim on stderr, or at the next SessionStart.
- `memory dream` — archives durable-file overflow and consolidates; it never moves entries to home. `memory promote`
  is the only route to home.
- `memory index` — regenerates the `MEMORY.md` bank region and `memory/index.md`, rotates sessionHistory, commits the
  store. **Never hand-edit generated regions.** `index`, `dream`, `update`, `import` and plain `review` also
  refresh the cached session-start notice (`state/harness/memory/doctor-notice-<store>`); session start only reads it.
- The embedding cache (`state/harness/memory/embeddings.db`) is one cache per embedding model, shared by every
  Databricks workspace: the profile only picks the endpoint (a failing one pauses a minute, the next serves). A model
  change re-embeds; a damaged or truncated file is set aside as `.damaged` and rebuilt empty.
- `memory review` — caps, staleness, hot/cold. `memory promote <id> --to home|rules|skill|claude-md` — `home`
  moves the block (no link back); `rules` and `skill` print the block, target and procedure and edit nothing;
  `claude-md` does the same only inside `memory dream`'s own run and is refused anywhere else (`promoteJudgment`).
  The ladder: Promotion ladder above.
- `memory init` scaffolds a fresh repo's store (also minted on first use).

The store's git repo is never pushed and nothing in it rides a project's commits.

## ADK mapping

| ADK | This harness |
| --- | --- |
| `preload_memory` | `memory-start` |
| `load_memory` | `claude-code memory search [--repo R \| --all] <terms>`, or `Read` a file |
| `add_session_to_memory` | `memory update` / `memory watch` |
| `add_events_to_memory` | `memory-compact` |
| `MemoryEntry` | One dated/titled block in a bank file |
| Compaction | `memory index` rotation; `memory digest` summarizes the archive into `archive/digest.md` (one haiku call, never digest-of-digest) |
