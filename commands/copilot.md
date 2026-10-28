---
description: "Dispatch the copilot agent: `test` (default) repairs and writes tests, `commit`/`pr` do the last mile; any other `claude-code copilot <verb>` (`ask`, `review`, `resume`, `status`, …) passes straight through"
argument-hint: "[test|commit|pr|ask|resume|review|explore|transfer|status|tail|result|cancel|setup|catalog|env] <args>"
allowed-tools: Agent, Bash
---

# /copilot

Launcher only. Modes, verbs and the report contract live in `agents/copilot.md`; `claude-code copilot -h`
lists the surface.

First token of `$ARGUMENTS`: `test` (default) or `commit`/`pr` — dispatch one `Agent` with
`subagent_type: "copilot"`, in the background, briefed with that mode, the rest as the task plus the files
or packages in scope and the exact verification command; relay its report. Any other verb
(`ask|resume|review|explore|transfer|status|tail|result|cancel|setup|catalog|env`) — run
`claude-code copilot <verb> <rest>` straight through via Bash and relay the output
(`--background` → relay the `job <id>` line and name `claude-code copilot status|result|cancel`).
