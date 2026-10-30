---
description: "Full-lifecycle coding via the claude agent — implement, review, or simplify the current change."
argument-hint: "[review | simplify] [diff range / path / blank for current diff]"
allowed-tools: Bash, Read, Grep, Glob, Edit, Write, Agent
---

Launcher only. Design requests go to the `Plan` agent. Modes, the phase sequence, and the output format live in
`agents/claude.md`.

Read the mode from `$ARGUMENTS`'s first token (`review`, `simplify`; blank defaults
to `review`). The remaining args are the scope: an explicit diff range, a path, or blank for the
current diff (`git diff ${RANGE:---cached}` against `git merge-base HEAD main`, falling back to
`git diff --cached` with a note if there's no divergence and nothing staged).

Dispatch one `Agent` with `subagent_type: "claude"`, passing the mode, resolved scope, and changed
files (not this session's history); the leaf does its own work (bare label). For a `review`, the
Security lens runs with the quality lenses. Relay its verdict.
