# Plan: Title of the change

Suggested slug: <slug>

## Context

Why this change, what it must achieve, what is out of scope.

## Phases

### Phase 1 — <verb> <object>

**Files:** `path/to/file.go:120-145` (modify), `path/to/new.go` (create)
**Change:** the exact edit, with the code — functions, signatures, data flow.
**Verify:** live check `<command>` → expected output; then `go test ./internal/<pkg> -run '<Name>'`.
**Agent / parallel:** `leaf: <code-worker|claude|…>, Agent: <requested agent or in-context>` (or
`Workflow <name>`); parallel-safe with Phase N, or depends on Phase M.
A phase that is itself a pipeline (implement+verify, audit a range, loop a gate) names the saved Workflow as its agent
(`skills/subagent-workflows/references/saved-workflows.md`); the manager runs it only with the user's opt-in.

### Phase 2 — <verb> <object>

…

## Risks / decisions for the user

…

## Tasks

A phase is a group of tasks, not a unit of work: each `### Phase` section lists its task ids. Every task is
leaf-sized — one package (or ≤3 tightly coupled files), ≤10 findings or edits, fits one 45-minute leaf — and
names its `leaf:` (a `subagent_type`, `code-worker` for MEDIUM+ changes), `Agent:` (requested agent or in-context)
and `owns:` files (`skills/subagent-workflows/SKILL.md` -> Leaf types), e.g.:
`- [ ] Phase 2.1 — Add --attach to copilot — leaf: code-worker, Agent: opencode — owns: agent/engine_cli.go`

Tick `[x]` with the commit id as each item lands (`- [x] Phase 2.1 — … (abc1234)`); this list is the recovery
point if a session, agent or worktree is lost. Mark an item **you** when only the user can do it. For a
multi-lane split, see `skills/subagent-workflows/references/lane-orchestrator.md`.

- [ ] Phase 1.1 — <name> — leaf: <subagent_type>, Agent: <agent> — owns: <files>
- [ ] Phase 2.1 — <name> — leaf: <subagent_type>, Agent: <agent> — owns: <files>
- [ ] Live check on the real surface
- [ ] Scoped tests for the touched packages
- [ ] Review (`claude-code codex review --adversarial --base <branch>`)
- [ ] Full suite once
- [ ] Commit (local; no push)
- [ ] Handoff / `claude-code memory update`
