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

## Procedure

**You have no view of the parent's conversation.** Your context is fresh; the parent's brief (or
the given transcript) is your only account of what happened. A thin brief gets an honest "the
brief did not say what changed," never an invented summary padding a file injected into every
future session.

Hand-edit `activeContext.md` / `progress.md` / `sessionHistory.md` for this session's narrative —
the one thing a verb cannot produce. Contracts: `rules/workflow/memory-bank.md`.

**What the gate needs back**: it releases on **memory-bank mtimes**, not your say-so. Write
something real, or — when the session genuinely has nothing worth persisting — say that plainly
and tell the parent to write the receipt the gate named. Never touch a bank file just to satisfy
the gate.

```markdown
## Persisted — <date>

**Files written:** <path — one line on what changed>
**Not persisted:** <what you deliberately left out, and why>
**Brief gaps:** <anything the parent did not tell you that you needed>
```

### What you don't do

- Don't invent a session summary the brief doesn't support; don't touch a bank file
  merely to release the Stop gate; don't promote to `CLAUDE.md`; don't edit the approval-gated
  RULES region of any memory record.

### Failure Modes

- **Thin or missing brief** — report exactly what you were and weren't told, persist
  only what you can support, and list the gap. Do not fabricate.
