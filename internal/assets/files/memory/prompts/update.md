# `claude-code memory update` — Judgment Residue: the Session Narrative

What Go cannot generate is the narrative: what actually happened this session, in your own
account. The bank is injected every session, so keep it to what is wrong-and-dangerous to forget;
don't fatten it with facts a lookup can serve.

File names below are bare; edit the exact paths the prompt lists (under the store root from `claude-code memory path`).

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

Go already filed every recorded DECISION event as a dated entry; never re-add one. File each other decision of the
session (a listed DECISION tag, or an untagged one the digest shows) unless decisionLog.md already has it:
`claude-code memory remember --type project '<decision> — <rationale>'`.
Supersede, never delete: a decision that replaces an older one carries `--supersedes <old id>` (for a Go-filed entry,
Edit a `Supersedes <old id>.` line under it); the harness strikes the old one.

## Step 5: Patterns → `conventions.md`

Append genuinely NEW recurring patterns or user preferences (imperative, one bullet each; NOTE tags land here or in
progress). No platitudes; nothing already in CLAUDE.md or `rules/` (point there instead).

Prefer non-obvious discoveries, each with the paths it applies to:

- Coupled files: a change in one silently requires a change in another.
- Hidden execution paths: what really runs (a hook, a generated file, a wrapper) differs from what the code shows.
- Misleading errors: the message points away from the real cause; record the cause.
- Verified workarounds: a tool quirk and the fix that worked this session.

Omit standard language or framework behavior.

## Rules

Edit bank files with Edit/Write only; Bash is read-only except `memory remember` (with or without `--supersedes`):
no `printf >>`, heredocs or Python writes.

Your context is fresh: the transcript digest is your only account of the session. Never invent a summary it
does not support; when nothing is worth persisting, write nothing rather than touch a file. Don't promote to
`CLAUDE.md` or edit the approval-gated RULES region of any memory record. Contracts: `rules/workflows/memory-bank.md`.
