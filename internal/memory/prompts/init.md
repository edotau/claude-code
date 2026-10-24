# `claude-code memory init` — Judgment Residue: Enrichment

What Go cannot do after scaffolding the six typed files is fill their placeholders with content
specific to this repo — that's this reference, an OPTIONAL enrichment pass on top. Skip any
content that already exists — never overwrite; leave sections empty rather than inventing content.

| File | Seed with |
|---|---|
| `projectContext.md` | One-paragraph orientation + pointers to canonical docs + any do-not-regress invariants |
| `activeContext.md` | Current Focus (branch, task), empty Open Questions/Blockers, Next Steps, Parked |
| `progress.md` | What Works / In Progress / What's Next / Known Issues from current repo state |
| `decisionLog.md` | Header + contract note only (append-only, newest first, supersede-never-delete) |
| `conventions.md` | Header + contract note only |
| `sessionHistory.md` | Header + contract note only (cap 30; overflow auto-archives to `archive/`) |

Every file keeps its `# Title` and one-line `>` mutation-contract note from the scaffold. Finish by
reporting the files touched and reminding that `session-start` injects the bank automatically and the
Stop/SessionEnd harvest (or `claude-code memory update`) saves it.
