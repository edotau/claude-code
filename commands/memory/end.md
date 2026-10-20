---
description: "Session end — save state to the memory bank: summary, active context, progress, decisions, conventions"
---

# /memory:end — Save the Session to the Memory Bank

> Bank dir: `claude-code memory path`. The `session-harvest` Stop hook runs this in the background
> (`claude-code memory update`) once a session has ≥3 file edits; run it by hand to save now or to
> recover a harvest the session-start note reports as failed.
>
> The background job runs on the Copilot CLI (claude-opus-5.5, Claude fallback); a manual run overrides it.

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
  `archive/sessionHistory-<yyyy-mm>.md`, rewrites this repo's `MEMORY.md` bank region, and
  regenerates the store-root catalog (`~/.claude/memory/index.md`). Deterministic and safe to
  re-run; do NOT hand-edit the bank region or the catalog yourself.
- Hold the line caps (`internal/memory/store.go`): projectContext 60, activeContext 80, progress 60,
  conventions 80, decisionLog 200.
- The bank lives in `~/.claude/memory/` — its own local-only git repo, never pushed and never
  part of the project's own commits. Settling (`memory index`) commits there automatically; don't
  commit it from the project's working tree.
- Report one block: files touched, history entry title, decisions/conventions added, carry-forwards.

## Procedure (from agents/opencode.md)

The writer the `session-harvest` Stop gate asks for — a Stop hook is a subprocess and cannot
call the Agent tool itself, so the gate blocks precisely so a live model can dispatch you.

**You have no view of the parent's conversation.** Your context is fresh; the parent's brief is
your only account of what happened. A thin brief gets an honest "the brief did not say what
changed," never an invented summary padding a file injected into every future session. If given
a transcript path, read it; otherwise work from the brief.

**Drive the CLI, don't improvise file writes.** Prefer the deterministic verbs over hand-editing:

```bash
claude-code memory update --transcript <path> --cwd <repo> --session <id> # session-end update: /memory:end + index
claude-code memory index     # regenerate the bank's routing index
claude-code memory doctor    # caps, staleness, cross-tier duplication
claude-code memory path      # resolve the bank paths; never hardcode them
```

Hand-edit `activeContext.md` / `progress.md` / `sessionHistory.md` only for content no verb
produces — this session's narrative. Contracts: `rules/workflow/memory-bank.md`.

**What the gate needs back**: it releases on **memory-bank mtimes**, not your say-so. Write
something real, or — when the session genuinely has nothing worth persisting — say that plainly
and tell the parent to write the receipt the gate named. Never touch a bank file just to satisfy
the gate.

```markdown
## Persisted — <date>

**Verbs run:** <claude-code memory ...>
**Files written:** <path — one line on what changed>
**Not persisted:** <what you deliberately left out, and why>
**Brief gaps:** <anything the parent did not tell you that you needed>
```

### What you don't do

- **PERSIST**: don't invent a session summary the brief doesn't support; don't touch a bank file
  merely to release the Stop gate; don't promote to `CLAUDE.md`; don't edit the approval-gated
  RULES region of any memory record.

### Failure Modes

- **Thin or missing brief (PERSIST)** — report exactly what you were and weren't told, persist
  only what you can support, and list the gap. Do not fabricate.
- **A verb fails (PERSIST)** — report the command and its stderr verbatim; don't hand-write what
  the verb was supposed to produce.
