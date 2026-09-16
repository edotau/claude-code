---
description: "GitHub workflow: commit, pr, review, or switch account — one command, mode as first arg"
argument-hint: "commit [msg] | pr | review | account <gh-user>"
allowed-tools: Bash
---

# /github — GitHub workflow (⇐ merged /git:* + /gh:*)

First arg is the mode; the rest are that mode's args. Default (no mode) = `review`.

## `commit [message]`
1. `git status` + `git diff --stat` to see scope.
2. Stage specific changed files by name — **never `git add -A`** (could include `.env`/secrets).
3. Message: use `$ARGUMENTS` if given, else generate conventional-commit form (feat|fix|refactor|
   docs|test|chore|perf|ci; imperative; ≤72 chars). No AI-attribution footers.
4. Confirm with the user before committing.

## `pr`
0. **Pre-flight** — detect toolchain, run lint+test; STOP and report on failure (do not commit):
   `pyproject.toml`→`ruff check . && pytest` · `package.json`→`npm run lint && npm test` ·
   `Makefile`→`make lint && make test` · `Cargo.toml`→`cargo clippy && cargo test` ·
   `go.mod`→`go vet ./... && go test ./...`
1. `git status` + `git diff` to review.
2. Stage the appropriate files; commit (conventional-commits format).
3. Push, creating the remote branch if needed (`-u origin <branch>`).
4. `gh pr create` — clear title + body (summary of what/why, testing done, reviewer notes).
Stop and report on any error.

## `review`
1. `git status` + `git diff`.
2. Per modified file: correctness/completeness, bugs, convention fit, security, error handling.
3. Summarize: what's good, concerns/suggestions, next steps (test | commit | revise).

## `account <gh-user>`
Switch the GitHub CLI identity: `gh auth switch --user <gh-user>`, then `gh auth status` (first 3
lines only). With no user, list the logged-in accounts from `gh auth status` and ask which one.

## Task

$ARGUMENTS
