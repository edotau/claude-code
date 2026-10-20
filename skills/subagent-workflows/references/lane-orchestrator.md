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

**Step 0, bug fixes only:** capture RED before dispatch — `scripts/isolate.sh /tmp/claude-1000/<lane>/red`
with no owned paths is pure HEAD; reproduce the failure there. After the leaf runs the worktree already holds the fix.
Remove it after: `scripts/isolate.sh --remove /tmp/claude-1000/<lane>/red`.

Run each leaf in the background (Bash `run_in_background`). Orchestrator files live in `/tmp/claude-1000/<lane>/`;
the leaf's own scratch is a separate `/tmp/claude-1000/<leaf>/` it may delete:

```bash
mkdir -p /tmp/claude-1000/<lane>; s=$(date +%s)
claude-code ask --agent <runner> --fallback none --tools --autonomous --attach <lane.md> --timeout 45m \
  --workdir <worktree> "<brief>" > /tmp/claude-1000/<lane>/<leaf>.out 2>&1
echo "<leaf> exit=$? wall=$(( $(date +%s)-s ))s" >> /tmp/claude-1000/<lane>/status
```

- `--fallback none`: the default `all` silently re-runs a failed leaf on claude, hiding which runner did the work.
- Wait in one foreground Bash call (≤10m, repeat as needed):
  `until [ "$(grep -c exit= <status> 2>/dev/null || echo 0)" -ge N ]; do timeout 5 tail -f /dev/null; done`.
- Codex rate-limited: substitute `--agent copilot --model gpt-6-astra`.
- Never start a test-repair leaf before its implementer's exit line lands.
- Leaves that own disjoint files run concurrently; a leaf whose files overlap another's waits for it.
- One-line tasks finish in under a minute; a small leaf still running at ~10m needs a look, not more waiting.

Brief template (the attached lane plan carries the context, so the brief does not restate it):

```text
You are a terminal leaf: never dispatch, commit, or git stash/checkout/reset/restore.
Task: <the checklist line, verbatim>. Do only this line of the attached lane plan.
Edit only: <owned files>. Scratch: /tmp/claude-1000/<leaf>; delete it when done.
Done when: <scoped test command> exits 0 and <live check> shows <expected>.
Report: files changed, each command with its exit code and the env it ran under (cwd, CLAUDE_CONFIG_DIR),
anything you found or needed outside your files.
```

A leaf's report is advisory (Copilot often returns prose with no exit codes); only your isolated check counts.
A finding outside the leaf's files becomes a new task line in this lane if it is the lane's package, else a note to
the manager.

## Verify each leaf

1. From the worktree: `scripts/isolate.sh /tmp/claude-1000/<leaf>/copy <owned paths>` — a `git worktree add` at
   HEAD with only this leaf's owned-path changes applied on top ([isolate.sh](../../../scripts/isolate.sh)).
2. In the copy (it has its own `.git`, a real worktree): build, vet, scoped tests
   (`go build ./... && go vet <pkgs> && go test <pkgs>`). Commit only on exit 0; gate on the exit code itself,
   never on a piped `| tail`.
3. The leaf's live check: the verb or flow it changed, on real input.
4. Read the diff: no weakened or deleted assertions, no edits outside `owns:`, no leftover references to removed
   names (`grep -rn <old name>`).
5. In the worktree: `git add <new files>; git commit -m "<msg>" -- <files>`, named files only (never a directory).
   A pathspec commit is `--only`, so other lanes' staged files never ride along; 1–2 line conventional message,
   no AI footer.
6. Tick the box with the short commit id; remove the copy (`scripts/isolate.sh --remove <copy>`) and the leaf
   scratch (`chmod -R u+w <d> && find <d> -delete`).

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
