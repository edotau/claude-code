---
description: "Create and manage git worktrees for parallel Claude Code sessions"
argument-hint: "<create|list|merge|clean|clean-all> [name] [--branch <b>] [--target <ref>] [--dry-run|--no-clean] [--verify \"<cmd>\"|--no-verify] [--resolve-agent claude|codex|--resume]"
---

# worktree -- Git Worktree Lifecycle Manager

> Single source of truth for worktree creation, listing, merging, and cleanup.
> Used directly or delegated to by `/workflow:orchestrate`.
>
> **Native overlap**: `create`/`enter` overlap the built-in `EnterWorktree`/`ExitWorktree`
> tools — prefer those inside a session. This command's genuinely additive value is the
> **`merge`** and **`clean-all`** lifecycle (branch merge-back, bulk stale-worktree pruning)
> that the native tools don't cover. Fan-out Agent dispatches work in the shared tree on
> disjoint files rather than per-agent worktrees.

**Input**: $ARGUMENTS

Current branch:
```
!git branch --show-current
```

Existing worktrees:
```
!git worktree list 2>/dev/null
```

**Location** — one root shared by native Claude Code (the `worktree-create` hook), Orca, and this
command: `~/worktrees/<repo>/<slug>`, `<repo>` = the main checkout's basename. Every subcommand below
— including `merge` — delegates to `claude-code worktree …` (`internal/worktree`), which resolves this
root and every worktree's actual branch from git itself — never assume a naming convention
(`wt/<name>`, `worktree-<name>`, an Orca-chosen name): discover it. `merge` runs a two-phase,
fast-forward-only promotion (`internal/worktree/preflight.go`, `merge.go`): a predicted conflict stops
before anything mutates unless `--resolve-agent claude|codex` is passed (`resolve.go`), which hands it
to an agent inside the worktree only; `--resume` reattaches to that operation after a crash.

---

## PARSE SUBCOMMAND

| Input | Action |
|-------|--------|
| `create <name> [--branch <b>]` | Create worktree + branch `worktree-<name>` (or an existing branch `<b>`) |
| `<name>` *(no subcommand)* | Same as `create <name>` |
| `list` | Show all worktrees with status |
| `merge <name> [--target <ref>] [--no-clean] [--verify "<cmd>"\|--no-verify]` | Preview (plan + predicted conflicts), integrate, run the verify gate (explicit, auto-detected, or skipped with `--no-verify`) before promoting, merge back fast-forward-only, auto-clean unless `--no-clean` |
| `merge <name> --resolve-agent claude\|codex` | On a predicted conflict only, resolve it with that agent inside the worktree, then continue the normal merge |
| `merge <name> --resume` | Reattach to a previously started `--resolve-agent` operation; never re-invokes the agent |
| `clean <name>` | Remove a single worktree safely |
| `clean-all` | Remove all `~/worktrees/<repo>/*` worktrees |

If no arguments at all, run `list`.

---

## SUBCOMMAND: `create <name>`

Create a new worktree for a parallel Claude Code session — delegates the git plumbing to
`internal/worktree.Create`, the same function the `worktree-create` hook calls.

1. Derive a kebab-case slug from `<name>` (e.g., "add auth endpoint" -> `add-auth-endpoint`).

2. Create (or resume) the worktree. No `--branch` reuses `worktree-$SLUG` — the hook's naming,
   now shared by every creation path — if it already exists; resuming an existing `$SLUG`
   worktree is safe (it prints the same path rather than recreating):
   ```bash
   WT=$(claude-code worktree create "$SLUG" ${BRANCH:+--branch "$BRANCH"})
   ```
   A refusal here (bad name, traversal, or a foreign directory already occupying `$SLUG` — a
   basename collision) prints the reason and stops; do not retry with `-f` or similar.

3. Python repos only — symlink the shared `.venv` (avoids a 500MB+ copy):
   ```bash
   [ -d .venv ] && ln -sf "$(pwd)/.venv" "$WT/.venv"
   ```

4. Copy environment files:
   ```bash
   cp .env "$WT/.env" 2>/dev/null || true
   cp .claude/settings.local.json "$WT/.claude/settings.local.json" 2>/dev/null || true
   ```

5. Print: path, branch, base SHA, and the follow-ups — launch via `cd $WT && claude`, merge back via `/workflow:worktree merge $SLUG`, clean via
   `/workflow:worktree clean $SLUG`. Inside the harness repo (`~/.claude`) itself, the banned git verbs
   (hard rule 4) are allowed inside this worktree — it is a separate checkout — but `make install` and
   `claude-code` verbs there still target the live `~/.claude` unless `CLAUDE_CONFIG_DIR` points at it.

---

## SUBCOMMAND: `list`

Show all worktrees under `~/worktrees/<repo>/` with status — delegates to `internal/worktree.List`,
which unions git's own registration with the directories actually on disk and inspects each one live
(dirty/merged/stale/orphaned/prunable/locked), reading its real branch rather than assuming one:

```bash
claude-code worktree list
```

Each row already carries a quoted `cd` to reopen the worktree and, only when the live recheck
says it's eligible, the exact `claude-code worktree clean` command to remove it — present those
verbatim rather than re-deriving `$WT`/`$SLUG` by hand. If no worktrees exist under
`~/worktrees/<repo>/`, it prints that and stops.

---

## SUBCOMMAND: `merge <name> [--target <ref>] [--no-clean]`

Merge a worktree branch back onto the main checkout's current branch — delegates to
`internal/worktree.Preflight` / `IntegrateTarget` / `Promote`, which pin both OIDs, simulate the
merge with `git merge-tree` before touching anything, integrate the pinned target **inside the
worktree**, run the verify gate there, and promote with a fast-forward-only `git merge --ff-only`
on the main checkout — never a `--no-ff` merge commit on the live branch, and never a real conflicting
merge. `Promote` also refuses to fast-forward onto anything but the exact OID `IntegrateTarget` just
verified: a commit added to the worktree between integration and promotion stops with a retry hint
instead of silently riding along unreviewed.

**The verify gate is required by default** — the CLI itself picks it, nothing here needs to discover
one: `--verify "<cmd>"` sets it explicitly, otherwise it auto-detects from the worktree (Makefile test
target, go.mod, pyproject, package.json, Cargo.toml). Neither explicit nor detected refuses to promote,
before anything is integrated, with `no verification gate found — pass --verify "<cmd>" or --no-verify`.
`--no-verify` is the only way to promote unverified.

1. **Preview** — always run first, never skipped:
   ```bash
   claude-code worktree merge --dry-run "$SLUG"
   ```
   Prints the pinned target/source OIDs and every `issue:` found. A clean plan prints
   `clean — no predicted conflicts` and exits 0; any issue (dirty checkout, detached target, a
   `--target` mismatch, unrelated histories, a `merge-tree` capability failure, or a predicted
   `conflict` naming the conflicting files) exits 1 and nothing has mutated — skip to the **Merge
   blocked** block below and stop. A `conflict` issue is the one Appendix A Phase 3 resolves with an
   agent; until then, report it rather than retrying with a flag — there is none.

2. On a clean preview, present it to the user and ask: "Merge $SLUG into \<target\>? (Y/n)".

3. On confirmation, apply — pass `--verify "<cmd>"` only to override auto-detection, or `--no-verify`
   to skip it on purpose:
   ```bash
   claude-code worktree merge "$SLUG"
   ```
   This re-runs Preflight itself (never trusts the earlier preview), integrates the pinned target into
   the worktree, runs the verify gate there, and only on success fast-forwards the main checkout onto
   the result — a failing gate stops here: the worktree is preserved on disk, untouched by promotion,
   so the user can inspect it, fix the gate, or retry with `--verify`/`--no-verify`. On success it then
   promotes and — unless `--no-clean` was passed — removes the worktree and deletes the branch exactly
   as `clean` would (the same live recheck, not a forced removal). A failure after promotion prints a
   retry hint instead of leaving a merge commit on the live branch; re-run Step 1 to get a fresh plan
   before retrying.

4. **Report** (success path):
   ```
   Merge complete
   ==============
   Branch: worktree-$SLUG -> <target>
   Verify: <the --verify command and its pass/fail>
   ```
   On `--no-clean`, the command's own `worktree left on disk; clean up with: ...` line replaces any
   separate cleanup prompt — present it verbatim rather than re-deriving the command.

---

### Merge blocked (Step 1 issue)

Print the exact `issue:` line(s) from the dry-run preview and stop — do NOT run Step 3. For a
predicted `conflict`, its `Paths` names the conflicting files:

```
Merge blocked
=============
issue: conflict [<files>] — git merge-tree predicts a conflict; Phase 3 adds agent resolution for this

The worktree is still on disk, untouched. To clean up without merging:
  /workflow:worktree clean $SLUG
```

---

### Agent-assisted resolution (`--resolve-agent claude|codex`)

A predicted `conflict` — and only that issue, never a dirty/detached/mismatched checkout — can be
handed to an agent instead of stopping:

```bash
claude-code worktree merge --resolve-agent codex "$SLUG"
```

This materializes the predicted conflict for real **inside the worktree** (`git merge`, never on the
main checkout), saves an `Operation` record naming every originally-conflicted path, and runs the
named agent once, with tools enabled, to edit those files only — the agent is told not to run `git
add`/`commit`/`merge` itself (a sandboxed agent, e.g. codex's workspace-write, cannot write inside
`.git` anyway). Once the agent exits cleanly with no fallback, the **controller** verifies every one of
those paths lost its conflict markers and that nothing outside them changed, then runs `git add` +
`git commit --no-edit` itself inside the worktree and revalidates the result (both the pinned target
and the worktree's prior HEAD as ancestors of the new HEAD, rejecting a stale merge commit). On success
it continues straight into the normal integrate/promote/clean path above. Markers left behind or an
unexpected path changed fails the operation outright, the same as a timeout — the operation record and
the worktree's edits stay on disk, nothing is cleaned up, so the state can be inspected or resumed. A
codex leg that silently fell back to Claude, a timeout, or cancellation (Ctrl-C) fails the same way.
`--resume` falls back to polling the worktree every two seconds, up to ten minutes, for the rare case a
human (or an agent that committed anyway) finishes the job out of band.

To reattach after a controller crash or Ctrl-C, without ever re-invoking the agent:

```bash
claude-code worktree merge --resume "$SLUG"
```

This reloads the saved `Operation` and revalidates the worktree's current state from scratch; if it is
already a clean, committed resolution it proceeds straight to promote, otherwise it reports exactly
why (still mid-merge, aborted, or a stale merge commit) and stops again.

---

## SUBCOMMAND: `clean <name>`

Safely remove a single worktree — delegates to `internal/worktree.Clean`, which rechecks clean,
merged, unlocked and owned state live immediately before removing (never trusts a prior `list`)
and never forces.

1. Preview first:
   ```bash
   claude-code worktree clean --dry-run "$SLUG"
   ```
   A refusal (uncommitted changes, unmerged HEAD, locked, or inspection failure) prints the exact
   reason — surface it and stop; do not retry with `--force` or similar. There is no such flag.

2. On a clean preview, ask for confirmation, then apply:
   ```bash
   claude-code worktree clean "$SLUG"
   ```
   This removes the worktree and, only if a plain `branch -d` succeeds (branch fully merged, no
   force), deletes the branch too — an unmerged branch is kept on purpose.

3. Print the command's own result line (`removed: …` / `branch deleted: …`, or `refused: … (reason)`).

---

## SUBCOMMAND: `clean-all`

Remove every eligible worktree under `~/worktrees/<repo>/` — delegates to
`internal/worktree.CleanAll`, which runs the same live recheck `clean` does, per worktree.

1. Preview:
   ```bash
   claude-code worktree clean-all --dry-run
   ```
   Prints every worktree as `eligible:` or `refused: … (reason)`, plus a summary count.

2. Present that summary to the user and ask: "Remove N worktrees? (Y/n)"

3. On confirmation, apply:
   ```bash
   claude-code worktree clean-all --yes
   ```

Unregistered directories under the root and git's own `prunable` administrative entries are
reported (by `list` and by `clean-all --dry-run`'s underlying listing) but never auto-removed in
this phase — surface them to the user, do not delete them.
