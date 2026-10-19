---
paths:
  - "internal/memory/**"
  - "commands/memory/*.md"
  - "commands/reflection/*.md"
---
# Memory Bank — Session-to-Session Store (`~/.claude/memory/<repo>/`)

> One store: `~/.claude/memory/` is a local-only git repo (`memory/index.md` is the generated
> catalog; `memory/<repo>/` is this repo's dir — the harness itself is `memory/.claude/`; the
> store top is global). Each repo dir holds BOTH Claude Code's native auto-memory (`MEMORY.md` +
> topic files) and six typed bank files, loaded automatically at session start (`session-start`
> hook) and saved at session end (`/memory:end`, encouraged by the `session-harvest` Stop hook).
> The bank holds session STATE — durable harness/project FACTS belong in `rules/` or `CLAUDE.md`.

## Files + mutation contracts

| File                | Holds                                                               | Contract                                                                  |
| ------------------- | ------------------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| `projectContext.md` | Orientation + do-not-regress invariants; points at CLAUDE.md/README | Replace, sparingly                                                        |
| `activeContext.md`  | Current focus, open questions, blockers, next steps, parked list    | **Replace** each session end                                              |
| `progress.md`       | What Works / In Progress / What's Next / Known Issues               | **Replace** each session end                                              |
| `decisionLog.md`    | Dated decisions + rationale                                         | **Append-only, newest first; supersede, never delete** (strike + pointer) |
| `conventions.md`    | Learned patterns/preferences not yet promoted to rules/CLAUDE.md    | Append; `/reflection:consolidate` prunes                                  |
| `sessionHistory.md` | Newest-first session summaries                                      | Append at top; overflow rotates to `archive/sessionHistory-<yyyy-mm>.md` via `claude-code memory index` |

Each typed file carries `harness_kind: bank` frontmatter. `MEMORY.md` beside them is native and
carries a generated `<!-- bank:begin -->`…`<!-- bank:end -->` region (≤40 lines/4KB) that points
at the six typed files plus the newest entries — lines outside that region are Claude's own and
are never rewritten by `memory index`.

## Who writes what

- **`session-start` hook** (`internal/hooks/`): read-only — injects the catalog (`memory/index.md`)
  plus this repo's `activeContext.md` Current Focus/Blockers/Next Steps and `progress.md` In
  Progress; everything else loads on demand.
- **`memory-recall` hook** (UserPromptSubmit): injects up to a few matching bank/native-memory
  blocks for prompts with enough distinct terms; routes to another repo's store only when the
  prompt names it (catalog key or origin basename); silent otherwise.
- **`/memory:end`**: the primary writer at session close — summary → sessionHistory; rewrite
  activeContext + progress; decisions → decisionLog; patterns → conventions.
- **`session-harvest` / `session-harvest-end` hooks** (Stop / SessionEnd): after enough file
  edits, dispatch a worker that runs `claude-code memory update` (the headless `/memory:end`
  pass) so a session that forgets to save still gets persisted. The first dispatch also claims one
  detached `claude-code memory watch` per session: once the transcript has been quiet 10 min with
  edits no dispatch covered, it runs the same worker — a crash or kill fires no SessionEnd.
- **`claude-code memory index`**: regenerates this repo's `MEMORY.md` bank region and rotates
  `sessionHistory.md` past its cap into `archive/`; also regenerates the store-root catalog.
  GENERATED — never hand-edit the bank region or `memory/index.md`.
- **`claude-code memory doctor`**: reports cap usage and staleness per bank.
- **`claude-code memory promote`**: runs the same settle pass as `index` (rotate, region, catalog, commit);
  after the store cutover there is no auto-promotion to global. `/memory:promote` is the judgment layer.
- **`claude-code memory alias --repo <checkout> --into <key>`**: folds a second checkout's store into its canonical one.

## Scope — repo dir and global

- **Repo store** — `memory/<repo>/`. Minted by `claude-code memory init` (or on first use); the
  config dir itself resolves to `memory/.claude/`.
- **Global store** — `memory/` itself (the tree top), used by any non-repo cwd and reachable
  from every session via the catalog and `--all`/cross-repo search.

## Doctrine

- Commits: the memory store is its own git repo (`memory/.git/`), separate from the project's —
  every settle (`memory index`) commits there; it is never pushed, and nothing in it rides the
  project's own commits.
- Never duplicate CLAUDE.md/rules content into the bank; point at it. Promotion out of
  `conventions.md`/`decisionLog.md` goes through `/reflection:promote` or `/memory:promote`.
- A fresh repo scaffolds its store with `claude-code memory init` (or `/memory:init`).

## ADK concept mapping

For readers familiar with Google's Agent Development Kit memory services, this maps roughly:

| ADK concept | This harness |
| --- | --- |
| `preload_memory` | SessionStart hook: catalog + this repo's focus (Current Focus/Blockers/Next Steps/In Progress) |
| `load_memory` | `claude-code memory search [--repo R \| --all] <terms>`, or `Read` a typed/topic file directly |
| `add_session_to_memory` | The harvest pass (`/memory:end`, `claude-code memory update`, or `memory watch`) |
| `MemoryEntry` | A typed block (one dated/titled entry in a bank file) addressable by file + heading |
| Compaction | `claude-code memory index` rotation/archive, scoped to the repo (no automatic cross-repo promotion); `memory digest` summarizes the raw archive into `archive/digest.md` (one bounded haiku call, never digest-of-digest) |
| `add_events_to_memory` | `pre-compact` hook: dispatches the harvest for edits no earlier harvest covered, before Claude Code compacts |

## Native project memory — same dir, not a sibling

Claude Code's own native auto-memory (`MEMORY.md` + topic files) is **not a separate system**: it
lives in the same `memory/<repo>/` dir as the typed bank files. `~/.claude/projects/<slug>/memory`
is a harness-maintained symlink into that dir (created by `memory migrate`, repaired at
SessionStart) — there is no independent native tree to conflate with or diverge from the bank.
`/reflection:*` curates the native MEMORY.md + topics; `/memory:*` curates the typed files; both
read and write the same directory.
