# Lane Orchestrator

Many leaf-sized tasks across disjoint packages: the manager splits them into lanes; one Claude subagent per lane fans
out leaves through `claude-code agents ask` (outside the Agent-tool cap; `ask`'s per-account cap still applies),
verifies each in the shared tree, and commits it.

| Role | Who | Does | Never |
|---|---|---|---|
| Manager | main session | splits lanes by disjoint packages, dispatches one `claude` leaf per lane, merges, runs the suite once with approval | edits a lane's files |
| Lane orchestrator | that subagent | writes the lane checklist, dispatches and verifies each leaf, commits named files, ticks the box | edits outside its lane |
| Leaf | a `claude` leaf (or an agent-named type) that requests `Agent: <x>` via `claude-code agents ask` | one checklist line, owned files only | commits or dispatches subagents |

## Lane plan

`lanes/<lane>.md` beside the plan (`rules/harness/docs-placement.md`), holding only a checklist (line format and leaf
size: `agents/Plan.md` → `## Tasks`; 25–40-item dispatches timed out, 5–10 finished):

```markdown
- [ ] <verb> <object> — Agent: <copilot|codex|gemini|opencode|claude> — owns: <files>
- [x] <verb> <object> — Agent: copilot — owns: internal/foo/bar.go — a1b2c3d
```

## Dispatch

Bug fixes start with the live before/after run (`testing-guidelines.md`). Run the lane's disjoint leaves as one batch,
one JSONL line per leaf (`id`, `agent`, `profile`, `model`, `prompt`, `workdir`, `tools`, `timeout`; `profile`
omitted = the pin, `dev|qa|prod` = the all-Databricks arm):

```bash
claude-code agents ask --batch <lane>.jsonl --jobs 12 --fallback none --autonomous --attach <lane.md> --format json \
  > $TMPDIR/<lane>/batch.out    # one envelope per line; per-account caps and isolation apply
```

Overlapping leaves go in a later batch; new tests wait for "landed". Brief per line:
[prompts.md#lane-leaf](prompts.md#lane-leaf), lane plan attached. A finding outside the leaf's files is a new line here
or a note to the manager.

## Verify each leaf

1. In the shared tree; with the user's yes, `worktree create <name>` (at HEAD: copy the leaf's owned files in first).
2. `go build ./... && go vet <pkgs> && go test <pkgs>` (exit code, never `| tail`), then the leaf's live check.
3. Read the diff: assertions intact (`testing-guidelines.md` → When a test fails), none outside `owns:`, no old names;
   a changed seam: run every consumer's tests ([Fan-in](../SKILL.md#fan-in)).
4. `git add <new files>; git commit -m "<msg>" -- <files>` (a pathspec commit, so other lanes' staged files never ride
   along); tick the box with the short id; `worktree clean <name>` if one was made; delete the leaf scratch.
5. Red: retry once ([Fan-in](../SKILL.md#fan-in)) or fix a small gap in-lane; else leave it unticked.
