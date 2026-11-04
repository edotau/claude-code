# Sharing One Checkout Without a Worktree

Whenever several sessions share one tree (always `~/.claude`; any repo where you declined a worktree), they and
their leaves edit the same tree on `main`. Git sees one working copy, so nothing stops two sessions writing the
same file; collisions surface as a red build or a commit that carries another session's half-done hunks.

## Before planning

- List what else is live: `git status --short` (uncommitted files you didn't write), other sessions'
  `plans/*.md`, and `ListAgents` for peers in the same checkout.
- Check each task's `owns:` files against that list. Registry-shaped files (`internal/schemas/ledger_test.go`,
  `internal/settings/hookspec.go`, `internal/providers/defaults.json`) are edited by every lane that adds an
  entry; assume they overlap.
- Name each overlap in the plan's Risks with the holder and who goes first, or message the peer to agree an
  order before either edits.

## While editing

- Edit only your `owns:` files; a build break in someone else's file is reported to its owner, not fixed.
- Lint autofixes on save can rewrite your line without its import (`perfsprint` turned `fmt.Errorf` into
  `errors.New`): rebuild after every edit, not just at the end.

## Claims: who has uncommitted edits in a file

Every Write/Edit is recorded per session and leaf; a claim lasts while the file is dirty, its owner is live and the
file has no newer commit. Before you edit a claimed file the hook says who holds it; staging it (`git add`,
`git commit -a`/paths) asks, and blocks in a leaf; `git add -p` passes. Inspect by hand:
`tail -n 20 ~/.claude/state/harness/hooks/claims/*.jsonl`. Not covered: edits made by Bash (`sed -i`, formatters) and
by external agents (codex/copilot/opencode coworkers) — check `git diff <file>` before staging, as below.

## Registry files: the orchestrator owns them

Files every lane appends to (`internal/schemas/ledger_test.go`, `internal/schemas/testdata/registry.golden`,
`internal/settings/hookspec.go`, `internal/hooks/io.go`'s handler map, `rules/harness/hooks.md`) are never in a
leaf's `owns:`. The leaf's report carries its entry as a fenced block (file, anchor line, exact text); main applies
every lane's entries in one commit after the leaves land. Leaves share the tree by default; one that must compile
against a new entry before main applies it gets its own linked worktree (`claude-code worktree create <slug>`).

## Before committing

- `git diff <file>` on every file you commit: commit by path takes the WHOLE file, including a peer's hunks.
- A peer's hunk in your file: commit without that file and tell the peer, or agree they commit it first.
- Check the commit stands alone: `git archive HEAD | tar -x -C "$(mktemp -d)"`, then in that dir
  `go test -count=1` for the touched packages. A pass in the live tree can lean on uncommitted files.

## Never rewrite shared history

Every session in the checkout shares one HEAD: an amend rewrites *their* commit too. `safety-scan` blocks
`commit --amend`, `rebase` and `reset <commit>` in a shared tree: `~/.claude`, a tree a peer session works in, or a
main tree where your session has live leaves (a leaf there is always blocked). Fix a bad commit with a new commit or
`git revert`; to rewrite history, do it in your own linked worktree (`claude-code worktree create <slug>`), pushed
or not. Path resets (`git reset -- <file>`), `rebase --continue`/`--abort` and an infra error (with a warning) pass.

## Incident (2026-11-04)

One session amended another's pushed `8ca4f03c` into local `76a11afa` (folding in its own two files): origin
stayed intact, local `main` diverged, and any commit by either session would have stacked on the rewrite.

## Incident (2026-10-30)

A cleanup lane and the agent refactor both edited `internal/schemas/writers_test.go`. A commit by path swept the
refactor's three unpushed registry keys into `a32139a6`, and `origin/main` went red on `TestWriters` until the
refactor's commit landed. A clean-archive check without `-count=1` had passed; the same check with it failed.
