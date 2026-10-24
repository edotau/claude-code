# Lane Orchestrator

Many leaf-sized tasks across disjoint packages: the manager splits them into lanes; one Claude subagent per lane
fans out vendor leaves through `claude-code ask` (which does not count against the 8-agent inflight cap), verifies
each leaf in an isolated copy, and commits it.

| Role | Who | Does | Never |
|---|---|---|---|
| Manager | main session | splits lanes by disjoint packages, dispatches one `claude`/`code-worker` per lane, merges, runs the suite once | edits a lane's files |
| Lane orchestrator | that subagent | writes the lane checklist, dispatches and verifies each leaf, commits named files, ticks the box | edits outside its lane |
| Leaf | `copilot`, `codex`, `gemini`, `opencode` or `claude` via `claude-code ask` | one checklist line, owned files only | commits or dispatches |

## Lane plan

`plans/<date>-<slug>/lanes/<lane>.md`, holding only a checklist:

```markdown
- [ ] <verb> <object> — runner: <copilot|codex|gemini|opencode|claude> — owns: <files>
- [x] <verb> <object> — runner: copilot — owns: internal/foo/bar.go — a1b2c3d
```

Leaf size: one package (≤3 tightly coupled files), ≤10 edits, one 45-minute run; split anything bigger (25–40-item
legs timed out, 5–10 finished). Runners per [cost-routing.md](cost-routing.md).

## Dispatch

Bug fixes first capture RED with no copy: `git show HEAD:<file>` to scratch, mapped over the live file with
`go test -overlay <overlay.json>`. Then run each leaf in the background:

```bash
iso="${CLAUDE_CONFIG_DIR:-$HOME/.claude}/skills/subagent-workflows/scripts/isolate.sh"
mkdir -p /tmp/claude-1000/<lane>; s=$(date +%s)
claude-code ask --agent <runner> --fallback none --tools --autonomous --attach <lane.md> --timeout 45m \
  --workdir <worktree> "<brief>" > /tmp/claude-1000/<lane>/<leaf>.out 2>&1
echo "<leaf> exit=$? wall=$(( $(date +%s)-s ))s" >> /tmp/claude-1000/<lane>/status
```

Wait in one foreground call (≤10 min, repeat):
`until n=$(grep -c exit= <status> 2>/dev/null); [ "${n:-0}" -ge N ]; do timeout 5 tail -f /dev/null; done`.
Disjoint leaves run concurrently; overlapping ones wait; a test-repair leaf starts only after its implementer's exit
line. A small leaf still running at ~10 min needs a look, not more waiting.

Brief (the attached lane plan carries the context):

```text
You are a terminal leaf: never dispatch, commit, or git stash/checkout/reset/restore.
Task: <checklist line, verbatim>. Edit only: <owned files>. Scratch: /tmp/claude-1000/<leaf>; delete it when done.
Done when: <scoped test> exits 0 and <live check> shows <expected>.
Before a behavior change write INTENT: code does X; check expects Y; spec says Z (Y vs Z disagree = stop, report).
After a defect fix write TWINS: searched P, found N.
Report: files changed, each command with exit code and env (cwd, CLAUDE_CONFIG_DIR), INTENT/TWINS, anything outside your files.
```

A finding outside the leaf's files becomes a new line in this lane (same package) or a note to the manager.

## Verify each leaf

1. `"$iso" /tmp/claude-1000/<leaf>/copy <owned paths>` — a worktree at HEAD plus only this leaf's changes.
2. In the copy: `go build ./... && go vet <pkgs> && go test <pkgs>`; gate on the exit code, never a piped `| tail`.
3. The leaf's live check on real input.
4. A changed seam (signature, argv/env builder, flag, file format): read producer and every consumer and run each
   consumer's tests — `go build ./...` alone has missed a broken consumer package.
5. Read the diff: no weakened or deleted assertions, nothing outside `owns:`, no leftover old names.
6. `git add <new files>; git commit -m "<msg>" -- <files>` (a pathspec commit, so other lanes' staged files never ride
   along); tick the box with the short id; `"$iso" --remove <copy>`; delete the leaf scratch.

Red at any step: re-dispatch once with the failure output, or fix a small gap in-lane; still red → report it unticked.

## Failure buckets (2026-10-20 run)

Classify a failed leaf before changing its brief or runner. Buckets from
[agent-trace-triage](https://github.com/WasayAbid/agent-trace-triage) (WasayAbid), MIT.

- **recovery_policy** — 10 min timeout killed legs → `--timeout 45m` on every `--tools` leaf; a turn cap hit with no
  report → ≤10 edits per leaf, and `git status` in its worktree rather than a retry.
- **tool_execution** — Copilot sessions died while polling → test-repair leaves start after the implementer's exit;
  `/tmp` (tmpfs) filled → per-leaf scratch, deleted when done.
- **tool_selection** — one whole-branch review leaf → narrow review leaves; a safety-guard change judged by reading
  the diff → execute the payloads against the old and new binaries.
- **tool_arguments** — `generate` with a non-default config dir wrote live vendor configs → read `--vendors` and
  `--dry-run` first; a test wrote the real `~/.claude/skills` → `HOME`/`CLAUDE_CONFIG_DIR` = `t.TempDir()`;
  `isolate.sh` given `/tmp/claude-1000` as the copy dir wiped every session's scratch → a fresh `mktemp -d` root.
- **state_orchestration** — opencode ignored `--workdir` and wrote the live tree → compare
  `git -C ~/.claude status --short` before and after; Copilot reports omit exit codes → rerun every command in the
  copy and gate on its exit code.
