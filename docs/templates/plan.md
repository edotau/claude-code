# Plan: Title of the change

Suggested slug: <slug>

Date: YYYY-MM-DD. Snapshot: `<sha>` (line numbers are as of that commit and drift; function names are the contract).

## Context

**Goal.** What the owner asked for, in their words, and the rulings it rests on (decision ids).
**Out of scope.** What this plan does not change, and where that work goes instead.
**Target shape, checked against the code.** Every assumption the design rests on, proven or refuted:

| Assumption | Verdict | Evidence |
| --- | --- | --- |
| <what the design needs to be true> | Holds / **Wrong** / Partly | `<grep, go list or file:line that shows it>` |

**Approaches.**

- DECISION: <chosen approach> — <why it wins: smallest change, safe to stop after any commit>
- Rejected: <alternative> — <the one reason it loses>

## File map (when the plan moves or splits files)

| File | Lines | Verdict | Stays | Goes to `<new home>` |
| --- | --- | --- | --- | --- |
| `path/to/file.go` | 120 | stays / moves / splits | <functions> | <functions> |

## Interface (when the plan adds or changes exports)

| Action | Replaces |
| --- | --- |
| `NewFunc(arg T) error` | the direct call at `path/to/file.go:42` |

## Phases

Every commit runs this gate:

```bash
set -o pipefail
go build ./... && go vet ./internal/<pkg>/...
```

### Phase 0 — Capture the baseline (no commit)

**Files:** none tracked; `/tmp/<slug>/`.
**Change:** build `go build -o /tmp/<slug>/cc ./cmd/claude-code`, then save the output and exit code of each read-only
command the plan touches (stdin from `/dev/null`) under `/tmp/<slug>/base/`.
**Verify:** every later commit reruns them on a fresh binary and `diff -r`s against `base/`; name each expected
difference, and re-capture when a peer's commit changes the output.
**Agent / parallel:** `leaf: code-worker, Agent: in-context`; before everything.

### Phase 1 — <verb> <object>

**Files:** `path/to/file.go:120-145` (modify), `path/to/new.go` (create)
**Change:** the exact edit, with the code — functions, signatures, data flow.
**Verify:** live check `<command>` → expected output; then `go test ./internal/<pkg> -run '<Name>'`.
**Agent / parallel:** `leaf: <code-worker|claude|…>, Agent: <requested agent or in-context>` (or `Workflow <name>`);
parallel-safe with Phase N, or depends on Phase M.

### Phase 2 — <verb> <object>

…

## Tests (when the plan moves, deletes or breaks tests)

| Test file | Lands in | Action |
| --- | --- | --- |
| `path/to/file_test.go` | `<pkg>` | none / simple fix / move / **delete** (listed in the debt report) |

## Risks / decisions for the user

- **D1 (before Phase N): <question>.** Options, the cost of each, and the recommendation.
- **Risk: <what can go wrong>.** What catches it.
- NEXT: <follow-up left out of this plan>
- **Unverified.** What no check in this plan covers.

## Tasks

A phase is a group of tasks, not a unit of work: each `### Phase` section lists its task ids. Every task is
leaf-sized — one package (or ≤3 tightly coupled files), ≤10 findings or edits, fits one 45-minute leaf — and
names its `leaf:` (a `subagent_type`, `code-worker` for MEDIUM+ changes), `Agent:` (requested agent or in-context)
and `owns:` files (`skills/subagent-workflows/SKILL.md` -> Leaf types). A commit that touches every `owns:` file ticks
the task (`docs-plan-tick`); after the first full path, a bare name resolves in the previous entry's directory, e.g.:
`- [ ] Phase 2.1 — Add --attach to copilot — leaf: code-worker, Agent: opencode — owns: agent/engine_cli.go`

Tick `[x]` with the commit id as each item lands (`- [x] Phase 2.1 — … (abc1234)`); this list is the recovery
point if a session, agent or worktree is lost. Mark an item **you** when only the user can do it. For a
multi-lane split, see `skills/subagent-workflows/references/lane-orchestrator.md`.

- [ ] Phase 0.1 — Capture the baseline — leaf: code-worker, Agent: in-context — owns: /tmp/<slug>
- [ ] Phase 1.1 — <name> — leaf: <subagent_type>, Agent: <agent> — owns: <files>
- [ ] Phase 2.1 — <name> — leaf: <subagent_type>, Agent: <agent> — owns: <files>
- [ ] **you** — Decide D1 before Phase N
- [ ] Live check on the real surface
- [ ] Scoped tests for the touched packages
- [ ] Review (`claude-code codex review --adversarial --base <branch>`)
- [ ] Full suite once
- [ ] Commit, `make install`, push per task (named files only)
- [ ] Handoff / `claude-code memory update`
