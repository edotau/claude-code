---
description: "Auto-fix lint with the CLI fixers, then fix what they cannot yourself; for an agent dispatch use `make lint-fix`"
argument-hint: "[go|md|all]"
disable-model-invocation: true
model: sonnet
context: fork
allowed-tools: Bash(make lint*), Bash(golangci-lint *), Bash(git diff*), Read, Edit(**/*.go), Edit(**/*.md)
---

# /lint-fix

Launcher only. The fixers and checks live in the Makefile (`lint-fix` target).

Arguments: `$ARGUMENTS` (go, md or all; empty means all). Run `make lint-fix NOAGENT=1 WHAT=<that value>`.
Exit 0 (`clean after auto-fix`) — report that and stop.
Otherwise fix each residual in the printed output yourself, then re-run until it is clean or a pass removes no issue
(at most 3 passes). Rules: prefer a real fix over `//nolint` (a `//nolint` needs a reason comment); never edit a
file with a `// Code generated` header; reuse an existing constant before adding one; edit only the files the
output names; run no git commands beyond `git diff`. Report what changed and what remains.
