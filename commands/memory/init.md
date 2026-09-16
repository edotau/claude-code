---
description: "Scaffold or enrich the session memory bank for this repo"
---

# /memory:init — Enrich the Memory Bank

Structure comes from the Go scaffold (`claude-code memory init`, or the `session-start` auto-init for
a git toplevel, a dir with `.claude/`, or `~/.claude` itself) — the single writer of the six typed files,
at `claude-code memory path`. This command is the OPTIONAL enrichment pass on top: fill the scaffolded
placeholders from the repo's CLAUDE.md/README and git state. Run `claude-code memory init` first if the
bank is absent. Skip any content that already exists — never overwrite; leave sections empty rather
than inventing content.

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
Stop/SessionEnd harvest (or `/memory:end`) saves it.
