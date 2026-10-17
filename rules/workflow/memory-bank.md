---
paths:
  - "internal/memory/**"
  - "commands/memory/*.md"
  - "commands/reflection/*.md"
---
# Memory Bank — Session-to-Session Store (`docs/memory/bank/<repo-slug>/`)

> Typed session memory: six untracked, per-machine markdown files under
> `docs/memory/bank/<repo-slug>/` (gitignored — scaffold a fresh machine with `claude-code memory
> init`), loaded automatically at session start (`session-start` hook) and saved at session end
> (`/memory:end`, encouraged by the `session-harvest` Stop hook). This bank holds session STATE —
> durable harness/project FACTS belong in `rules/` or `CLAUDE.md`, not here.

## Files + mutation contracts

| File                | Holds                                                               | Contract                                                                  |
| ------------------- | ------------------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| `projectContext.md` | Orientation + do-not-regress invariants; points at CLAUDE.md/README | Replace, sparingly                                                        |
| `activeContext.md`  | Current focus, open questions, blockers, next steps, parked list    | **Replace** each session end                                              |
| `progress.md`       | What Works / In Progress / What's Next / Known Issues               | **Replace** each session end                                              |
| `decisionLog.md`    | Dated decisions + rationale                                         | **Append-only, newest first; supersede, never delete** (strike + pointer) |
| `conventions.md`    | Learned patterns/preferences not yet promoted to rules/CLAUDE.md    | Append; `/reflection:consolidate` prunes                                  |
| `sessionHistory.md` | Newest-first session summaries                                      | Append at top; overflow rotates to `archive/sessionHistory-<yyyy-mm>.md` via `claude-code memory index` |

## Who writes what

- **`session-start` hook** (`internal/hooks/`): read-only — injects activeContext (minus its
  `## Parked` section, which memory-recall surfaces by topic) + progress + the newest sessionHistory entry.
- **`memory-recall` hook** (UserPromptSubmit): injects up to a few matching bank/native-memory
  blocks for prompts with enough distinct terms; silent otherwise.
- **`/memory:end`**: the primary writer at session close — summary → sessionHistory; rewrite
  activeContext + progress; decisions → decisionLog; patterns → conventions.
- **`session-harvest` / `session-harvest-end` hooks** (Stop / SessionEnd): after enough file
  edits, dispatch a worker that runs `claude-code memory update` (the headless `/memory:end`
  pass) so a session that forgets to save still gets persisted. The first dispatch also claims one
  detached `claude-code memory watch` per session: once the transcript has been quiet 10 min with
  edits no dispatch covered, it runs the same worker — a crash or kill fires no SessionEnd.
- **`claude-code memory index`**: regenerates the bank's routing index; rotates
  `sessionHistory.md` past its cap into `archive/`. GENERATED — never hand-edit.
- **`claude-code memory doctor`**: reports cap usage, staleness, and cross-tier duplication.
- **`claude-code memory promote`**: moves settled local overflow into the shared global bank
  (deduped). `/memory:promote` is the judgment layer above it — see that command.

## Two layers — local and global

- **Local working bank** — `docs/memory/bank/<repo-slug>/`. Minted only when the cwd is a git
  work tree; any other cwd collapses to the global bank.
- **Shared global bank** — `docs/memory/bank/` itself (the tree TOP), used by any non-repo cwd
  and read **alongside** the local bank on every session — so a session sees other projects'
  promoted lessons too.

## Doctrine

- Commits: memory updates ride the working branch like any other file — bank files themselves
  are gitignored and never committed.
- Never duplicate CLAUDE.md/rules content into the bank; point at it. Promotion out of
  `conventions.md`/`decisionLog.md` goes through `/reflection:promote` or `/memory:promote`.
- A fresh repo scaffolds the bank with `claude-code memory init` (or `/memory:init`).

## Relationship to native agent memory

`agent-memory/<name>/` (Claude Code's own `memory: user` scope) is a SIBLING system, not part of
the bank: it is per-agent, not per-session, and is curated by `/reflection:*` rather than
`/memory:*`. Don't conflate the two when writing a command or rule — the bank answers "where did
this session leave off", agent memory answers "what has this agent learned".
