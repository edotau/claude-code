---
name: using-git-worktrees
description: Current harness worktree verbs, lane landing and cleanup, shared-checkout coordination, and manual worktree mechanics. Triggers "git worktree", "parallel branches", "isolated workspace", "work on two branches", "worktree cleanup".
---

# Using Git Worktrees

Linked worktrees isolate working directories, not the shared Git object store or stash list.
Policy belongs to [Git and Worktrees](../../rules/standards/worktree-git.md); this skill documents mechanics.

## Verbs

Run `claude-code worktree -h`, then the verb's `-h` for flags. Put flags **before** the lane name
(`commit` also accepts interspersed flags). A lane name is its directory basename: `agent-<id>`, **not**
its branch `worktree-agent-<id>`. The verbs resolve the actual branch through Git.

| Verb                                               | Behavior / flags                                                                                                                                                                                                                                                                                                                                                                                  |
| -------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `create <name>`                                    | Prints `~/worktrees/<repo>/<name>`; defaults to branch `worktree-<name>`, or `--branch <b>`; reuses an existing owned worktree/branch. `<repo>` drops a leading dot.                                                                                                                                                                                                                              |
| `list`                                             | Status, ahead/behind, overlaps, reopen and cleanup commands; `--format json`. Exits 1 if any lane's divergence is unreadable, after printing the others.                                                                                                                                                                                                                                          |
| `gate`                                             | Prints the detected gate; `--dir`, comma-separated `--scope`, `--lint-check`, `--format json`. `--run` executes it and returns JSON: inspect `exit` (a red gate still gives CLI exit 0). No gate exits 1.                                                                                                                                                                                         |
| `commit`                                           | `-m <msg>` or `--type <t>`; named files or `@id` candidates (`--list`). Main may omit files for its own claims; dispatched leaves must name them. `--dry-run`, repeatable `--task <id>`, `--plan <file>`.                                                                                                                                                                                         |
| `land <name>`                                      | Rebase, verify, apply uncommitted; `--dry-run`, `--target <branch>`, `--no-clean`, `--verify <cmd>`, `--full-verify`, `--no-verify`, `--resolve-agent <agent>`, `--resume`.                                                                                                                                                                                                                       |
| `collapse <collector> <lane>...`                   | Cherry-pick each lane into the collector, build and vet per lane, then remove the lane (`git worktree remove`, branch deleted at the picked sha) when its head is unmoved, it has no uncommitted or untracked file and nothing runs in it; else it prints `kept: <why>`. Conflicting or failing lanes are dropped and kept. `--dry-run` prints "would remove". Cleanup happens here, at collapse. |
| `clean <name>`                                     | Remove an eligible worktree or its kept branch; `--dry-run`, `--force` (discard dirty/unmerged worktrees).                                                                                                                                                                                                                                                                                        |
| `clean-all`                                        | Preview eligible/refused lanes; only `--yes` applies, `--dry-run` overrides it.                                                                                                                                                                                                                                                                                                                   |
| `base <ref>` / `base --clear`                      | Pin/clear the current branch's status-line diff base.                                                                                                                                                                                                                                                                                                                                             |
| `pr list\|view\|diff\|create`, `checks`, `comment` | Origin-host PR/CI operations; use each subcommand's `-h` (the top-level `pr -h` shows the worktree overview).                                                                                                                                                                                                                                                                                     |

`create` and native `EnterWorktree` share `worktree-create` setup: `.venv` and `venv` get a local overlay reusing
primary packages with editable imports redirected to lane source, or a symlink fallback; Git excludes are added
if needed. `.env` is copied only if the destination branch ignores it. Existing destinations are not overwritten;
resumed worktrees are not reseeded; seed failures warn. See `internal/worktree/{seed,venvoverlay}.go`.

## The lane protocol

Follow the [canonical lane policy](../../rules/standards/worktree-git.md#worktrees). In particular,
`HARNESS_WORKTREE=lanes` is **advisory**, not an isolation boundary (see the enforcement table).

From a repo root, set `LANE` to an unused name and the other variables to an open task, dated plan and owned file.
Creation is real (there is no create dry-run); only the commit below previews:

```bash
: "${LANE:?set lane name}" "${TASK_ID:?set open task id}" "${PLAN:?set dated plan path}" "${OWNED_FILE:?set owned path}"
wt=$(claude-code worktree create "$LANE") && cd "$wt" &&
claude-code worktree commit --dry-run --task "$TASK_ID" --plan "$PLAN" -m "docs: clarify lanes" "$OWNED_FILE"
```

Remove `--dry-run` only at the authorized commit boundary. `--task` validates an open task before staging and
adds `Task: <plan-slug> <id>` trailers; `--plan` disambiguates. Task ticking is not a land operation; see the standard.
An Agent-tool **relay** leaf cannot commit, even in a worktree: leave the diff
for the dispatched executor or main session. `isolation:worktree` selects a checkout; it does not itself impose a
blanket read ban on the live checkout in these handlers. Reads remain subject to configured permissions.

From outside the finished lane:

```bash
claude-code worktree land --dry-run "$LANE" &&
claude-code worktree land "$LANE" &&
claude-code worktree clean --dry-run "$LANE"
```

Several ready lanes in the queue: `claude-code worktree land --drain` (no name) is the one drainer per checkout. It
picks the lanes into one `land-batch-*` collector, runs one gate, lands it, then prints `drain <lane>: ...` per lane.
A failed batch gate falls back to landing the lanes one by one; a lane whose pick conflicts stays queued. A second
drainer exits at once with exit 0 and changes nothing. A single `land <name>` is unchanged. The attempt log
(`land-attempts.jsonl`) carries `changed_pkgs`, `importer_pkgs` and, on a failed gate, `failed_pkgs`.

### Land: effects and recovery

The `land` verb enforces these steps (`internal/worktree/{preflight,land,gate,audit}.go`):

1. Pin the primary checkout's current branch and both OIDs; `--target` requires that branch, not an arbitrary
   destination. Refuse a dirty source, busy lane (process cwd inside), in-progress Git operation or other blocking
   preflight issue. A predicted conflict alone may proceed; a preview does not run the gate or prove it can land.
2. Rebase **inside the lane**, then gate the rebased tree. Makefile `test` + `go.mod` uses concurrent build/vet
   across the module and tests of changed packages plus **direct** importers (including test imports), not a fixed
   registry set. Non-Go files map to owners/embedders/tests naming them; `go.mod`/`go.sum` or unowned non-Markdown,
   non-Go files retain the full Makefile gate. `--full-verify` also retains it. Otherwise detection uses Makefile
   `test`, Go, Python, npm or Cargo. `--verify` overrides detection; no gate refuses unless `--no-verify`.
3. Recheck pinned heads, source state and liveness. A live foreign process with cwd inside, or a live dispatch job,
   refuses promotion (the invoking process's ancestry is excluded). If peers commit meanwhile, expect
   `promote: target moved ... since integration`: re-run land to integrate and verify again.
4. Check and apply the net diff to the primary working tree **unstaged and uncommitted**. Start a detached audit
   on the rebased lane head (equivalent to target HEAD plus the landed diff). Normally remove the worktree and
   keep its branch; `--no-clean`, a detached lane or a removal refusal keeps the worktree.

| Outcome                 | Meaning / next step                                                                                                                                                                                                                        |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Exit 3: rebase conflict | Rebase aborted, lane restored; resolve in the lane, then retry. `--resolve-agent <agent>` resolves via a controller-created merge commit; `--resume` reattaches without restarting the agent.                                              |
| Exit 3: land refused    | Overlapping dirty target paths, hidden index flags, unsupported submodule changes or failed patch check; live tree unchanged, lane kept. Resolve the named cause, retry.                                                                   |
| Exit 1                  | Other refusal/failure: gate red, moved target, busy lane, etc. A gate failure can leave the lane rebased; it does not promote. An apply failure **after** a clean check may partially write the live tree; inspect status before retrying. |
| Exit 0                  | Preview plan accepted (possibly with predicted conflicts), or apply succeeded/already landed. Preview never mutates, even on nonzero exit; after apply, read any worktree-removal refusal.                                                 |

`clean --dry-run <name>` rechecks ownership, cleanliness, merge/landed-diff state, locks and use by processes.
After the owner's commit, `clean <name>` can delete the kept branch if its changed paths match `HEAD`; it need not
be an ancestor. If they do not match, it refuses. `clean --force` discards an **existing worktree** and its branch,
but still refuses main, locked or in-use lanes; it does not handle a branch-only remainder. User approval before
intentional discard is policy, not a confirmation prompt in this verb. `clean-all` does not force-remove dirty,
unmerged, orphaned or prunable entries. Native `worktree-remove` likewise refuses in-use/dirty worktrees and only
tries safe `branch -d`; it is not the landed-diff-aware `clean` path.

### Hook enforcement vs. policy

Hooks run on the events/tools declared in `internal/settings/hookspec.go`, not on every possible filesystem write.

| Rule / process                                                                                                  | What actually enforces it                                                                                                                                                                                                                    |
| --------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Lane choice, dated plan before work, no stash, no shared-history rewrites, root sandbox scope, discard approval | Policy/prose in the standard and workflow; not a blanket guard in the hooks listed here. `safety-scan` applies configured denies, not a universal Git rewrite ban.                                                                           |
| Lanes-mode reminder                                                                                             | `worktree-claim-check` (`wthook.ClaimCheck`): notes tracked edits/stages in the live harness checkout and, once per session, another repo's primary checkout; never refuses merely for lanes mode.                                           |
| Peer's dirty claimed file                                                                                       | `worktree-claim-check`: note before edits; ask before stage/commit, block a leaf in the live config checkout. `worktree commit` independently refuses claimed peer files.                                                                    |
| Relay commit refusal                                                                                            | `dispatch-handback` / `relay-shell`; applies to Agent-tool relays, not every executor.                                                                                                                                                       |
| Finished leaf auto-land                                                                                         | `lane-land`: detached worker waits for completion and clean `UNFIXED: none`; dirty/busy/unreadable lanes queue. Its restriction check is path-based, not semantic proof of owner approval. `lane-land-queue` prints pending work on prompts. |
| Land / cleanup checks                                                                                           | `worktree land`, `worktree clean`, `clean-all` and native `worktree-remove`, as detailed above.                                                                                                                                              |
| Plan/report placement                                                                                           | `docs-placement` redirects/gates plans and blocks recognized misplaced Markdown writes; it does not require every task to have a plan. Placement policy: `rules/harness/docs-placement.md`.                                                  |
| No AI-attribution footers                                                                                       | `safety-scan` blocks recognized commit/merge/tag/PR message footers; commit policy stays in the standard.                                                                                                                                    |

Shared-tree claims and registry coordination: [shared-checkout.md](references/shared-checkout.md).
Manual creation and dev-server setup: [manual-worktrees.md](references/manual-worktrees.md).
