# Lane Orchestrator

Many leaf-sized tasks across disjoint packages: the manager splits them into lanes; one Claude subagent per lane
fans out agent leaves through `claude-code ask` (not counted by the dispatch cap:
`rules/workflow/agents.md` → Parallel dispatch), verifies each leaf in an isolated copy, and commits it.
Leaf types and the `Agent:` request:
[SKILL.md → Leaf types](../SKILL.md#leaf-types).

| Role | Who | Does | Never |
|---|---|---|---|
| Manager | main session | splits lanes by disjoint packages, dispatches one `code-worker` (or `claude`) per lane, merges, runs the suite once with user approval | edits a lane's files |
| Lane orchestrator | that subagent | writes the lane checklist, dispatches and verifies each leaf, commits named files, ticks the box | edits outside its lane |
| Leaf | a `code-worker` that requests `Agent: <x>` via `claude-code ask` (`copilot`, `codex`, `gemini`, `opencode` or `claude`) | one checklist line, owned files only | commits or dispatches (subagents; the `Agent:` request is allowed) |

## Lane plan

`lanes/<lane>.md` beside the plan (plan home: `rules/harness/docs-placement.md`), holding only a checklist:

```markdown
- [ ] <verb> <object> — Agent: <copilot|codex|gemini|opencode|claude> — owns: <files>
- [x] <verb> <object> — Agent: copilot — owns: internal/foo/bar.go — a1b2c3d
```

Leaf size and the checklist line format: `agents/Plan.md` → `## Tasks` (25–40-item dispatches timed out, 5–10 finished).
Agents per [cost-routing.md](cost-routing.md).

## Dispatch

Bug fixes: reproduce on the real surface first (the live before/after run is the RED). Then run each leaf in the
background:

```bash
iso="${CLAUDE_CONFIG_DIR:-$HOME/.claude}/skills/subagent-workflows/scripts/isolate.sh"
mkdir -p /tmp/claude-1000/<lane>; s=$(date +%s)
claude-code ask --agent <agent> --fallback none --tools --autonomous --attach <lane.md> --timeout 45m \
  --workdir <worktree> "<brief>" > /tmp/claude-1000/<lane>/<leaf>.out 2>&1
echo "<leaf> exit=$? wall=$(( $(date +%s)-s ))s" >> /tmp/claude-1000/<lane>/status
```

Wait in one foreground call (≤10 min, repeat):
`until n=$(grep -c exit= <status> 2>/dev/null); [ "${n:-0}" -ge N ]; do timeout 5 tail -f /dev/null; done`.
Disjoint leaves run concurrently; overlapping ones wait; test repair runs concurrently with its implementer; new
tests wait for "landed". A small leaf still running at ~10 min needs a look, not more waiting.

Brief: [prompts.md#lane-leaf](prompts.md#lane-leaf); the attached lane plan carries the context.

A finding outside the leaf's files becomes a new line in this lane (same package) or a note to the manager.

## Verify each leaf

1. `"$iso" /tmp/claude-1000/<leaf>/copy <owned paths>` — a worktree at HEAD plus only this leaf's changes.
2. In the copy: `go build ./... && go vet <pkgs> && go test <pkgs>`; gate on the exit code, never a piped `| tail`.
3. The leaf's live check on real input.
4. A changed seam (signature, argv/env builder, flag, file format): read producer and every consumer and run each
   consumer's tests — `go build ./...` alone has missed a broken consumer package.
5. Read the diff: assertions intact (`testing-guidelines.md` → When a test fails), nothing outside `owns:`, no
   leftover old names.
6. `git add <new files>; git commit -m "<msg>" -- <files>` (a pathspec commit, so other lanes' staged files never ride
   along); tick the box with the short id; `"$iso" --remove <copy>`; delete the leaf scratch.

Red at any step: re-dispatch once with the failure output, or fix a small gap in-lane; still red → report it unticked.

Failure buckets from a past run: recovery_policy, tool_execution, tool_selection, tool_arguments, state_orchestration
(`memory/claude-code/archive/progress-2026-10.md`).
