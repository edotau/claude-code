---
paths:
  - "internal/memory/**"
  - "skills/reflection/**"
---
# Memory Bank — Session-to-Session Store (`~/.claude/memory/<repo>/`)

`~/.claude/memory/` is one local-only git repo: `index.md` is the generated catalog, `memory/<repo>/` a repo's store
(the harness is `memory/claude-code/`, non-repo cwds use `memory/home/`). Each store holds Claude Code's native
auto-memory (`MEMORY.md` + topics) **and** six typed bank files — one directory, not siblings:
`~/.claude/projects/<slug>/memory` is a symlink into it, repaired at SessionStart. The bank holds session STATE;
durable facts belong in `rules/` or `CLAUDE.md` — point at them, never copy.

## Commands

| To… | Run |
| --- | --- |
| Find past entries | `claude-code memory recall <terms>` · `memory search [--repo R \| --all] <terms>` |
| Capture a correction, then route it into a rule | `claude-code memory remember <fact>` → `memory promote <id> --to rules` |
| Save the session (auto at Stop/SessionEnd) | `claude-code memory update` |
| Audit or consolidate | `claude-code memory review` · `memory dream [--dry-run]` |
| Keep MEMORY.md small | `claude-code memory archive [--split 20 --apply]` |

## Files + mutation contracts

| File | Holds | Contract |
| --- | --- | --- |
| `projectContext.md` | Orientation + do-not-regress invariants | Replace, sparingly |
| `activeContext.md` | Focus, open questions, blockers, next steps, parked | **Replace** each session end |
| `progress.md` | What Works / In Progress / What's Next / Known Issues | **Replace** each session end |
| `decisionLog.md` | Dated decisions + rationale | **Append-only, newest first; supersede (strike + pointer), never delete** |
| `conventions.md` | Learned patterns not yet promoted | Append; `memory dream` prunes |
| `sessionHistory.md` | Newest-first summaries | Append at top; past 30 rotates to `archive/sessionHistory-<yyyy-mm>.md` |

Typed files carry `harness_kind: bank` frontmatter. `MEMORY.md` carries a generated `<!-- bank:begin -->`…
`<!-- bank:end -->` region (≤40 lines/4KB); lines outside it are Claude's own and never rewritten.

## Who writes what

- `memory-start` (SessionStart) — read-only: injects the catalog + this repo's Current Focus/Blockers/Next Steps and
  In Progress; the rest loads on demand.
- `memory-recall` (UserPromptSubmit) — injects a few matching blocks; another repo's store only when the prompt names
  it.
- `claude-code memory update` — the session-close writer: summary → sessionHistory, rewrite activeContext + progress,
  decisions → decisionLog, patterns → conventions.
- `memory-harvest` / `memory-end` (Stop / SessionEnd) — after enough edits, run `memory update` headless so a session
  that forgets still persists; the first dispatch claims a detached `memory watch` that harvests after 10 quiet
  minutes (a crash fires no SessionEnd). `memory-compact` does the same before compaction.
- `memory index` — regenerates the `MEMORY.md` bank region and `memory/index.md`, rotates sessionHistory, commits the
  store. **Never hand-edit generated regions.**
- `memory review` — caps, staleness, dupes, hot/cold. `memory promote <id> --to home|rules|skill|claude-md` — moves
  the entry and links back. `memory alias --repo <checkout> --into <key>` — folds a second checkout's store.
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
