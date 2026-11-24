---
name: using-git-worktrees
description: Isolated git worktrees for parallel feature work, hotfixes, and plan execution — directory selection with gitignore safety checks, .venv symlink + .env copy setup, deterministic port allocation for parallel dev servers, and merged/stale cleanup. Triggers "git worktree", "parallel branches", "isolated workspace", "work on two branches", "worktree cleanup".
---

# Using Git Worktrees

Isolated working directories off one shared `.git` for parallel branches (feature, hotfix, PR review) without
stashing or switching.

## When to use a worktree (vs. staying put)

| Situation | Action |
|-----------|--------|
| Need 2+ branches open locally at once | New worktree per branch |
| Hotfix needed while feature branch is dirty | Dedicated hotfix worktree (no stash) |
| Human terminal or long-running background agent owning its own branch | One worktree per agent |
| Parallel fan-out leaves in one session | One lane per stream (`HARNESS_WORKTREE=lanes`, below); else one shared tree (references/shared-checkout.md) |
| Reproducing a bug on another branch | Temporary worktree, clean up same day |
| Quick local diff review only | Stay on current tree — no worktree |

Staying put while other sessions edit the same tree — the no-rewrite rule, file claims, registry files:
[references/shared-checkout.md](references/shared-checkout.md).

## In this harness: use `claude-code worktree` (the paved road)

The verbs handle everything with the harness conventions baked in:

```bash
claude-code worktree create add-auth-endpoint   # → ~/worktrees/<repo>/add-auth-endpoint, branch worktree-add-auth-endpoint
claude-code worktree list [--format json]        # all worktrees: status, +ahead -behind main, file overlaps
claude-code worktree gate --dir . --scope internal/worktree   # prints the gate for this scope (that dir) only; narrower than merge's
claude-code worktree land add-auth-endpoint      # rebase in the lane, gate, land uncommitted on the live checkout
claude-code worktree clean add-auth-endpoint     # safe single removal; also deletes a landed lane's kept branch
claude-code worktree commit -m "fix: x" <files>   # one pathspec commit of named files; no paths = the main session's own claims (a subagent or CLI leaf names its files)
```

More verbs and options:

| Verb | Does |
|------|------|
| `claude-code worktree create <name> [--branch <b>]` | New worktree, then the venv/env steps |
| `claude-code worktree land <name> [--merge] [--target <ref>] [--no-clean] [--verify "<cmd>"\|--no-verify\|--full-verify]` | Rebase in the lane, gate, then **Land on local (Recommended)**: uncommitted changes in your checkout, you commit; `--merge` is **Merge into working branch**: a committed fast-forward; `--dry-run` changes nothing |
| `claude-code worktree land <name> --resolve-agent <agent>` · `--resume` | Agent resolves a conflict in the lane; reattach |
| `claude-code worktree commit -m <msg> [<files>] [--dry-run]` | Conventional subject, files only; refuses a peer's file (a CLI leaf: any claimed file); never pushes |
| `claude-code worktree clean <name>` · `clean-all [--yes]` | `--dry-run` preview, confirm, apply |

`clean` takes `--force` (discard uncommitted and unmerged work); `clean-all` takes only `--dry-run` and `--yes`; every
other verb refuses with a reason instead. Only pass `--force` with the user's explicit go — it is a data-loss operation.
`land` refuses when `main` moved mid-verify (re-run it); `clean` refuses while a process has its cwd inside (`cd` out
or `ExitWorktree`). Inside a session prefer the native `EnterWorktree`/`ExitWorktree` for create/enter; the additive
value here is `land` and `clean-all`.

`create` makes the worktree under `~/worktrees/<repo>/<slug>` (`<repo>` drops a leading dot; older
`~/worktrees/.claude` lanes still list; outside the repo, shared with native Claude Code) via `git worktree add`, then
seeds a `.venv` symlink and an `.env` copy when the source exists, the destination is absent and the new branch
ignores the path (a seeding failure only warns). The full procedure, the fast-forward-only `--merge` rule and conflict
handling: [references/manual-worktrees.md](references/manual-worktrees.md) Part 1. In the harness repo itself, a
worktree here is a separate checkout exempt from CLAUDE.md hard rule 4 — but `make install` and `claude-code` verbs
run from it still target the live `~/.claude` unless `CLAUDE_CONFIG_DIR` points at it
(`rules/harness/settings-management.md` → Worktree render). Never run a worktree-built binary as the proxy.

## The lane protocol (`HARNESS_WORKTREE=lanes`)

One lane per stream of work, opened after its dated plan (R18); a lane lands on the live checkout uncommitted and
you commit it (`--merge` instead fast-forwards `main`). Vault
env `HARNESS_WORKTREE=lanes` makes
the claim check refuse a leaf's edit or stage of a tracked file in the live checkout (it asks the main session) and name
`claude-code worktree create <slug>`; a lane is never refused.

```bash
claude-code worktree create <slug>                           # lane at ~/worktrees/<repo>/<slug>, branch worktree-<slug>
claude-code worktree commit --task <id> -m "<msg>" <files>   # in the lane; a leaf names its files
claude-code worktree land <slug> --dry-run                   # every step and the overlaps with other lanes; changes nothing
claude-code worktree land <slug>                             # rebase in the lane, gate, land uncommitted on the live checkout
claude-code worktree land <slug> --merge                     # Merge into working branch: committed fast-forward of main
claude-code worktree land <slug> --no-clean                  # keep the lane for its next task
```

A finished leaf's committed lane lands by itself (`lane-merge` hook); one it cannot land is a `merge queue:` line.

What `land` does, in order:

- Rebases the lane onto `main` inside the lane; `main` is never rebased.
- Runs the scoped gate in the lane on the rebased tree (`--verify "<cmd>"` overrides, `--full-verify` runs the whole
  Makefile gate). With no detectable gate the land is refused unless `--no-verify`.
- Applies the lane's net diff to the live working tree, uncommitted and unstaged (new, deleted and renamed files
  included); if `main` moved meanwhile it refuses and says to re-run. A landed path already dirty in the live tree, or
  a patch that does not check, changes nothing, keeps the lane and exits 3 naming the paths (a write failure after a
  clean check exits 1 and may be partial; the lane stays).
- Removes the lane's worktree and prints `landed <lane>: N files uncommitted in <dir>; branch <b> kept` plus the diff
  stat; the branch is the only committed copy until you commit, and `worktree clean <lane>` deletes it once its diff is
  in `HEAD`. No plan ticks fire at landing: they tick when you commit with `Task:` trailers or run `plan task --done`.
- With `--merge` instead: fast-forwards `main`, ticks the plan tasks named by `Task:` trailers, notices every other
  lane left behind (naming overlapping files) and deletes the branch too.

Exit code 3 = the rebase conflicted (aborted, the lane exactly as it was, files named, the lane's session notified) or
the land was refused on a dirty path. Fix a conflict in the lane, never on `main`: edit the files and commit with
`claude-code worktree commit`, or let an agent do it with `land <slug> --resolve-agent <agent>` (it resolves with a
merge commit in the lane, so that one merge is not linear history; `--resume` reattaches), then run `land` again.
The lane that lands second resolves. Other non-zero exits are refusals with a reason (gate red, `main` moved).

Notices a lane sees (drained at its next prompt or `git` command):

| Notice | Do |
|--------|----|
| `main moved to <sha> (lane <x> merged)` (a `--merge`), naming overlapping files | Nothing yet: `land` rebases you; with overlaps, land soon |
| A land stopped on a conflict in your lane | Resolve in the lane as above, then re-run `land` |

After a green land the audit runs detached on `HEAD` plus the landed diff; once you commit, the main session installs
and pushes; a red audit is fixed forward with a new commit or a `git revert` landed the same way, never a reset.

Known gaps:

- A conflict only in a generated file (goldens, projections) is not regenerated automatically: do it in the lane.
- A `--resolve-agent` merge commit can conflict again if `main` moves before the next rebase.

Registry files: a new file writer is one row in its package's own `internal/schemas/testdata/writers/<pkg>.txt`, so
parallel lanes no longer collide; the other registry files still go through the orchestrator
([references/shared-checkout.md](references/shared-checkout.md)).

## Manual control / non-harness repos

When `claude-code worktree` doesn't apply, follow the full mechanics (Part 2) — directory selection,
gitignore safety, `.venv` symlink, port allocation for parallel dev servers, and gated
cleanup — in [references/manual-worktrees.md](references/manual-worktrees.md).

## Scoping a subagent below the root breaks git and the venv

Grant a subagent or sandbox the repo (or worktree) root, never a subdirectory: that hides `.git` and `.venv` and
causes import errors that look like regressions.
