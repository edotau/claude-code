---
description: "Dispatch the copilot agent: `test` (default) repairs and writes tests, `commit`/`pr` do the last mile; any other `claude-code copilot <verb>` (`ask`, `review`, `resume`, `status`, …) passes straight through"
argument-hint: "[profile=<p>] [model=<m>] [test|commit|pr|<verb>] <args>"
allowed-tools: Agent, Bash
---

# /copilot

Launcher only. Modes, verbs and the report contract live in `agents/copilot.md`; `claude-code copilot -h`
lists the surface.

`profile=<p>` / `model=<m>` anywhere in `$ARGUMENTS` (strip them first) become `--profile <p>` /
`--model <m>` on the `claude-code copilot <verb>` call or the routed `ask` (not on `profile`/`on`/`off`); a verb that
lacks the flag errors, so relay that. `<p>` is a vault profile (`dev`, `qa`, `prod`, …), `<m>` an alias or wire id.

First token of `$ARGUMENTS`: `test` (default) or `commit`/`pr` — dispatch one `Agent` with
`subagent_type: "copilot"`, in the background, briefed with that mode, the rest as the task plus the files
or packages in scope and the exact verification command; the leaf does its own work (bare label); relay
its report (a `profile=<p>` adds `Profile: <p>` to that brief instead). Any other verb
(`ask|resume|review|explore|transfer|status|tail|result|cancel|setup|catalog|profile|on|off`) — run
`claude-code copilot <verb> <rest>` straight through via Bash and relay the output
(`--background` → relay the `job <id>` line and name `claude-code copilot status|result|cancel`).
