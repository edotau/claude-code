---
description: "Full-lifecycle coding via the claude agent — architect, implement, review, or simplify the current change."
argument-hint: "[review | simplify | architect <task>] [diff range / path / blank for current diff]"
allowed-tools: Bash, Read, Grep, Glob, Edit, Write, Agent
---

Launcher only. Modes, the phase sequence, and the output format live in `agents/claude.md`.

Read the mode from `$ARGUMENTS`'s first token (`review`, `simplify`, `architect`; blank defaults
to `review`). The remaining args are the scope: an explicit diff range, a path, or blank for the
current diff (`git diff ${RANGE:---cached}` against `git merge-base HEAD main`, falling back to
`git diff --cached` with a note if there's no divergence and nothing staged).

Dispatch one `Agent` with `subagent_type: "claude"`, passing the mode, resolved scope, and changed
files (not this session's history). For a `review`, also run `/security-reviewer` in parallel for the OWASP
pass. Relay its verdict.
