# `claude-code memory update` — Judgment Residue: the Session Narrative

What Go cannot generate is the narrative: what actually happened this session, in your own
account. The bank is injected every session, so keep it to what is wrong-and-dangerous to forget;
don't fatten it with facts a lookup can serve.

## Step 1: Session summary → `sessionHistory.md`

Add a newest-first entry AT THE TOP (below the header/contract block):

```markdown
## YYYY-MM-DD — <one-line arc title>

- <what shipped / was decided / broke, 2–6 bullets; include PR numbers and branch names>
```

## Step 2: Rewrite `activeContext.md`

REPLACE contents (keep the header + contract note): Current Focus (task + branch), Open Questions,
Blockers, Next Steps, and the Parked list (carry forward untouched items; drop resolved ones).
Stale focus is worse than no focus — this file always reflects NOW. File every NEXT tag under Next Steps.

## Step 3: Rewrite `progress.md`

REPLACE: What Works (stable, verified), In Progress, What's Next, Known Issues. Move newly-stable
items into What Works; drop shipped items from What's Next. File every BUG tag under Known Issues.

## Step 4: Decisions → `decisionLog.md`

For each architectural/process decision made this session (every DECISION tag is one): add a dated entry
at the top (`## YYYY-MM-DD — <decision>` + 1–3 lines of rationale). Supersede, never delete: strike the old
entry's title line and point at the new one.

## Step 5: Patterns → `conventions.md`

Append genuinely NEW recurring patterns or user preferences (imperative, one bullet each; NOTE tags land here or in
progress). No platitudes; nothing already in CLAUDE.md or `rules/` (point there instead).

## Rules

Your context is fresh: the transcript digest is your only account of the session. Never invent a summary it
does not support; when nothing is worth persisting, write nothing rather than touch a file. Don't promote to
`CLAUDE.md` or edit the approval-gated RULES region of any memory record. Contracts: `rules/workflows/memory-bank.md`.
