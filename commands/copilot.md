---
description: "Dispatch the copilot agent: `test` (default) repairs and writes tests, `commit`/`pr` do the last mile"
argument-hint: "[test | commit | pr] <task, packages, or files>"
allowed-tools: Agent
---

# /copilot

Launcher only. Modes, rules and the report shape live in `agents/copilot.md`.

Dispatch one `Agent` with `subagent_type: "copilot"`, in the background. Brief it with the mode from the first
token of `$ARGUMENTS`: `test` (default) is TEST; `commit` or `pr` is LAST-MILE, that subsection. Pass the rest of
`$ARGUMENTS` as the task, plus the files or packages in scope and the exact verification command. Relay its report.
