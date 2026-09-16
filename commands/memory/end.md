---
description: "Session end — save state to the memory bank: summary, active context, progress, decisions, conventions"
---

# /memory:end — Save the Session to the Memory Bank

> Bank dir: `claude-code memory path`. The `session-harvest` Stop hook runs this in the background
> (`claude-code memory update`) once a session has ≥3 file edits; run it by hand to save now or to
> recover a harvest the session-start note reports as failed.

Persist the session so the next one resumes instead of rediscovering. The bank is injected every
session, so keep it to what is wrong-and-dangerous to forget; don't fatten it with facts a lookup can serve.

## Step 1: Session summary → `sessionHistory.md`

Add a newest-first entry AT THE TOP (below the header/contract block):

```markdown
## YYYY-MM-DD — <one-line arc title>

- <what shipped / was decided / broke, 2–6 bullets; include PR numbers and branch names>
```

## Step 2: Rewrite `activeContext.md`

REPLACE contents (keep the header + contract note): Current Focus (task + branch), Open Questions,
Blockers, Next Steps, and the Parked list (carry forward untouched items; drop resolved ones).
Stale focus is worse than no focus — this file always reflects NOW.

## Step 3: Rewrite `progress.md`

REPLACE: What Works (stable, verified), In Progress, What's Next, Known Issues. Move newly-stable
items into What Works; drop shipped items from What's Next.

## Step 4: Decisions → `decisionLog.md`

For each architectural/process decision made this session: add a dated entry at the top
(`## YYYY-MM-DD — <decision>` + 1–3 lines of rationale). Supersede, never delete: strike the old
entry's title line and point at the new one.

## Step 5: Patterns → `conventions.md`

Append genuinely NEW recurring patterns or user preferences (imperative, one bullet each). No
platitudes; nothing already in CLAUDE.md or `rules/` (point there instead).

## Step 6: Settle + report

- **Run `claude-code memory index`** — it rotates `sessionHistory.md` past 30 entries into
  `archive/sessionHistory-<yyyy-mm>.md` and rewrites the routing `index.md` session-start injects.
  Deterministic and safe to re-run; do NOT rotate or hand-edit `index.md` yourself.
- Hold the line caps (`internal/memory/store.go`): projectContext 60, activeContext 80, progress 60,
  conventions 80, decisionLog 200.
- The bank is untracked (`docs/memory/` is gitignored) and per-machine — do NOT commit it.
- Report one block: files touched, history entry title, decisions/conventions added, carry-forwards.
