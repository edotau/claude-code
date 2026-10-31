# Sharing One Checkout Without a Worktree

With `HARNESS_WORKTREE=off`, several sessions and their leaves edit the same tree on `main`. Git sees one working
copy, so nothing stops two sessions writing the same file; collisions surface as a red build or a commit that
carries another session's half-done hunks.

## Before planning

- List what else is live: `git status --short` (uncommitted files you didn't write), other sessions'
  `plans/*.md`, and `ListAgents` for peers in the same checkout.
- Check each task's `owns:` files against that list. Registry-shaped files (`internal/schemas/writers_test.go`,
  `internal/hooks/hookspec/hookspec.go`, `internal/providers/defaults.json`) are edited by every lane that adds an
  entry; assume they overlap.
- Name each overlap in the plan's Risks with the holder and who goes first, or message the peer to agree an
  order before either edits.

## While editing

- Edit only your `owns:` files; a build break in someone else's file is reported to its owner, not fixed.
- Lint autofixes on save can rewrite your line without its import (`perfsprint` turned `fmt.Errorf` into
  `errors.New`): rebuild after every edit, not just at the end.

## Before committing

- `git diff <file>` on every file you commit: commit by path takes the WHOLE file, including a peer's hunks.
- A peer's hunk in your file: commit without that file and tell the peer, or agree they commit it first.
- Check the commit stands alone: `git archive HEAD | tar -x -C "$(mktemp -d)"`, then in that dir
  `go test -count=1` for the touched packages. A pass in the live tree can lean on uncommitted files.

## Incident (2026-10-30)

A cleanup lane and the agent refactor both edited `internal/schemas/writers_test.go`. A commit by path swept the
refactor's three unpushed registry keys into `a32139a6`, and `origin/main` went red on `TestWriters` until the
refactor's commit landed. A clean-archive check without `-count=1` had passed; the same check with it failed.
