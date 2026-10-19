# Plan: <title>

## Context

Why this change, what it must achieve, what is out of scope.

## Phases

### Phase 1 — <name>

Files, functions, the change, how it is verified (live check first, then scoped tests).

### Phase 2 — <name>

…

## Risks / decisions for the user

- …

## Tasks

Tick `[x]` with the commit id as each item lands (`- [x] Phase 1 — parse input (abc1234)`); this list is the
recovery point if a session, agent or worktree is lost. Mark an item **you** when only the user can do it.

- [ ] Phase 1 — <name>
- [ ] Phase 2 — <name>
- [ ] Live check on the real surface
- [ ] Scoped tests for the touched packages
- [ ] Review (`claude-code ask --agent codex`)
- [ ] Full suite once
- [ ] Commit + push
- [ ] Handoff / `/memory:end`
