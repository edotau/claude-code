# Lane Orchestrator

Many leaf-sized tasks across disjoint packages: the manager splits them into lanes, one Claude subagent per
lane fans out vendor leaves through `claude-code ask`, verifies each leaf, and commits it.

## Roles

| Role | Who | Does | Never |
|---|---|---|---|
| Manager | main session | splits work into lanes by disjoint packages/files; dispatches one Claude subagent (`claude` or `code-worker`) per lane; merges, runs the full suite once | edits a lane's files |
| Lane orchestrator | that subagent | writes the lane plan, dispatches each leaf, verifies it, commits named files, ticks the box with the commit id; reports when every box is ticked | edits outside its lane |
| Leaf | `copilot`, `codex`, `gemini`, `opencode` or `claude` via `claude-code ask` | one checklist item, owned files only | commits, dispatches, `git stash/checkout/reset/restore` |

## Lane plan

`plans/<date>-<slug>/lanes/<lane>.md`, beside the parent plan, holds ONLY a checklist:

```markdown
## Tasks

- [ ] <verb> <object> — runner: <copilot|codex|gemini|opencode|claude> — owns: <files>
- [x] <verb> <object> — runner: copilot — owns: internal/foo/bar.go — a1b2c3d
```

**Leaf size:** one package (≤3 tightly coupled files), ≤10 findings or edits, fits one 45-minute leaf, names its
runner and owned files. Split anything bigger before dispatching: legs of 25–40 findings timed out, 5–10 finished.
Spread runners per [cost-routing.md](cost-routing.md).

## Dispatch

Run each leaf in the background (Bash `run_in_background`), output to its own file, exit line to the lane status file:

```bash
mkdir -p /tmp/claude-1000/<leaf>
claude-code ask --agent <runner> --tools --autonomous --attach <lane.md> --timeout 45m \
  --workdir <worktree> "<brief>" > /tmp/claude-1000/<leaf>/<leaf>.out 2>&1
echo "<leaf> exit=$?" >> /tmp/claude-1000/<lane>.status
```

- Codex rate-limited: substitute `--agent copilot --model gpt-6-astra`.
- Never start a test-repair leaf before its implementer's exit line lands.
- Leaves that own disjoint files run concurrently; a leaf whose files overlap another's waits for it.

Brief template (the attached lane plan carries the context, so the brief does not restate it):

```text
You are a terminal leaf: never dispatch, commit, or git stash/checkout/reset/restore.
Task: <the checklist line, verbatim>. Do only this line of the attached lane plan.
Edit only: <owned files>. Scratch: /tmp/claude-1000/<leaf>; delete it when done.
Done when: <scoped test command> exits 0 and <live check> shows <expected>.
Report: files changed, each command with its exit code, anything you needed outside your files.
```

## Verify each leaf

1. From the worktree: `scripts/isolate.sh /tmp/claude-1000/<leaf>/copy <owned paths>` — HEAD plus only this
   leaf's changes ([isolate.sh](../../../scripts/isolate.sh) reverts every other changed or untracked path).
2. In the copy (no `.git`; `/tmp` has a stray one, so set `GOFLAGS=-buildvcs=false`): build, vet, scoped tests
   (`go build ./... && go vet <pkgs> && go test <pkgs>`). Commit only on exit 0; gate on the exit code itself,
   never on a piped `| tail`.
3. The leaf's live check: the verb or flow it changed, on real input.
4. Read the diff: no weakened or deleted assertions, no edits outside `owns:`, no leftover references to removed
   names (`grep -rn <old name>`).
5. In the worktree: stage the named files only (never a directory); commit a 1–2 line conventional message, no
   AI footer.
6. Tick the box with the short commit id; delete `/tmp/claude-1000/<leaf>`.

Red at any step: re-dispatch the leaf once with the failure output, or fix a small gap in-lane; still red, report
the item to the manager unticked.

## Lessons from the 2026-10-20 run

- Raise the ask timeout: the 10m default killed 6 legs.
- Copilot sessions die while polling: dispatch test leaves after their implementers exit.
- Scratch lives under `/tmp/claude-1000/<leaf>` and is deleted when done: `/tmp` is a 16 GB tmpfs.
- Prove a safety-guard change by executing payloads against main's binary, not by reading the diff.
- Never run `generate` with a non-default config dir until you understand `--vendors` (it writes the live vendor configs).
- Prefer narrow review leaves, one area each, over one whole-branch review.
- The Agent-tool inflight cap is 8 machine-wide; lanes fan out via `claude-code ask`, which doesn't count against it.
