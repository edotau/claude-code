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

Every wave / collector commit runs this static gate (tests run once at the final landing step after the review loop):

```bash
set -o pipefail
rtk go build ./internal/<pkg>/... && rtk go vet ./internal/<pkg>/...
```

### Phase 0 — Capture the baseline (no commit)

**Files:** none tracked; `/tmp/<slug>/`.
**Change:** build `rtk go build -o /tmp/<slug>/cc ./cmd/claude-code`, then save the output and exit code of each
state-reporting command the plan touches (status, counts, test results; stdin from `/dev/null`) under
`/tmp/<slug>/base/`. For each state-changing command, record what it would do (command, target, expected effect)
without running it.
**Verify:** every later commit reruns the state-reporting commands on a fresh binary and `diff -r`s against `base/`;
name each expected difference, and re-capture when a peer's commit changes the output. State-changing commands are
checked by their recorded effect, not run.
**Agent / parallel:** `leaf: claude, Agent: in-context`; before everything.

### Phase 1 — <verb> <object>

**Files:** `path/to/file.go:120-145` (modify), `path/to/new.go` (create)
**Change:** the exact edit, with the code — functions, signatures, data flow.
**Verify:** live check `<command>` → expected output; then `rtk go test ./internal/<pkg> -run '<Name>'`.
**Agent / parallel:** `leaf: <claude|codex|copilot|gemini|opencode|…>`,
`Agent: <requested agent or in-context>`
(or `Workflow <name>`); parallel-safe with Phase N, or depends on Phase M.

### Phase 2 — <verb> <object>

…

## Waves

One message dispatches a whole wave; owned files are disjoint within it; a verifier leaf closes each wave.

| Wave | Packages | Owned files | Depends on | Width bound / what widens it | Verify (command → proof) |
| --- | --- | --- | --- | --- | --- |
| 1 foundation (adds only) | 1.1, 1.2 | `path/a.go`, `path/b.go` | none | <file overlap or dependency> / <split or hoist> | `rtk go vet ./internal/<pkg>/...` → exit 0 |

## Leaf rules

- Never revert a change you did not make; no `git commit`, `stash`, `checkout`, `reset` or `restore`.
- Reproduce before fixing; the live before/after run is the evidence; a regression test is an owner-approved proposal.
- Write the full report to `<store>/reports/`; return a verdict of at most 8 lines.

## Tests (when the plan moves, deletes or breaks tests)

| Test file | Lands in | Action |
| --- | --- | --- |
| `path/to/file_test.go` | `<pkg>` | none / simple fix / move / **delete** (listed in the debt report) |

## Risks

- **Risk: <what can go wrong>.** What catches it.
- NEXT: <follow-up left out of this plan>
- **Unverified.** What no check in this plan covers.

## Notes

General reminders that apply to every task, kept up top so they are seen before the work starts:

- [ ] Handoff — `claude-code memory update`
- [ ] Commit, `rtk make audit`, `rtk make install`, push per task (named files only)

`NOTE:` lines go here too, one per line; a note longer than 120 characters stays as one line, not wrapped.

## Decisions

Every owner choice in the plan lives here, nowhere else; phases and tasks cite it by D-id. Each is a checkbox item:
`- [ ]` open, ticked `- [x]` with `Answer: <pick> — <date>, <who>` when decided (`claude-code plan task D1 --done`);
the item stays (history in one place), and an open one keeps the plan waiting on the owner.

- [ ] D1 — <what the decision is>
  a) <option: effect, pros, cons, numbers>  b) <option> — …
  Today: <what happens now> · Recommended: <letter> — <why> · Blocks: <phases or waves that wait on it>
  Answer: —

## Tasks

A phase is a group of tasks, not a unit of work: each `### Phase` section lists its task ids. Every task is leaf-sized
— one package (or ≤3 tightly coupled files), ≤10 findings or edits, fits one 45-minute leaf — and names its `leaf:` (a
`subagent_type`, `claude` for MEDIUM+ changes), `Agent:` (requested agent or in-context) and `owns:` files
(`skills/subagent-workflows/SKILL.md` -> Leaf types). A commit names its task with `worktree commit --task <id>`
(ticking rules: `rules/harness/workflow-tree.md`; name a task only on its last commit).
Start with a repo path; later entries may abbreviate against the previous one: a bare name sits in its
directory, `<dir>/<file>` drops the shared leading part (`internal/configure/a.go, b.go, ui/c.go`). E.g.:
`- [ ] Phase 2.1 — Add --attach — leaf: opencode, Agent: opencode — owns: internal/agent/cli.go`

`claude-code agents ask --waves <plan>` reads the same lines: `Phase W.k` is wave W, package k;
`W.V` is the wave's manual verifier (never launched). Optional attributes go **before** `owns:`, which takes
everything after the last `owns:`:
`... Agent: codex — verify: go test ./internal/claude — findings: reports/x.md#key — owns: internal/claude/gate.go`.
`verify:` defaults to `claude-code agents diff --check` on the owned files.
`Blocked by: 1.1, Phase 1.2` (also before `owns:`) starts a task once those tasks are ticked or finished ok: a
same-wave blocker runs in an earlier round of the wave, an earlier wave's must be ticked or verified, a later wave's
open task, an unknown id or a cycle is refused before anything runs; a failed blocker skips the task, naming it.
Prose naming no task id (`Blocked by the option-B leaves`) holds the task for main; `--dry-run` shows each wait.
A tick runs `verify:` on the landed commit first; only `go build|vet|test ./…`, `claude-code agents diff --check` and
`claude-code doctor …` (and their `rtk` forms) run there. The wave gate is the first `bash` fence
under `## Phases` unless `--gate CMD` overrides it. Exit codes, reruns and the background launch:
`skills/subagent-workflows/references/agent-verbs.md`.

Tick `[x]` with the commit id as each item lands (`- [x] Phase 2.1 — … (abc1234)`); this list is the recovery
point if a session, agent or worktree is lost. Mark an item **you** when only the user can do it (a choice is a
`## Decisions` item, not a **you** line). For a multi-lane split, see
`skills/subagent-workflows/references/lane-orchestrator.md`.

- [ ] Phase 0.1 — Capture the baseline — leaf: claude, Agent: in-context — owns: /tmp/<slug>
- [ ] Phase 1.1 — <name> — leaf: <subagent_type>, Agent: <agent> — owns: <files>
- [ ] Phase 2.1 — <name> — leaf: <subagent_type>, Agent: <agent> — owns: <files>
- [ ] Live check on the real surface
- [ ] Basic tests that the feature works (a couple of seconds to run at most; more cases: proposals)
- [ ] Review — `claude-code codex review --adversarial --base <branch>`
- [ ] Fix review findings — `/code-review high --max-findings all --fix "<every finding>"` to the R28 bar
- [ ] Full suite once
