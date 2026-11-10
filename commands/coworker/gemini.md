---
description: "Run gemini on the Databricks arm: Databricks first, first-party login as the fallback"
argument-hint: "[profile=<p>] [model=<m>] [fallback=<f>] [effort=<e>] [tools=on|off|read-only] [host=<h>] <task>"
allowed-tools: Bash(claude-code agents ask --agent gemini --route *)
disable-model-invocation: true
---

# /coworker:gemini

`/coworker gemini …` with the agent fixed; arguments, defaults and examples: `~/.claude/commands/coworker.md`.

Launcher only. Empty `$ARGUMENTS` or `help`: print that file's usage block and stop. Otherwise turn each leading
`key=value` from it into its flag (an unknown key is task text) and run the ONE call
`claude-code agents ask --agent gemini --route databricks[:<p>] [flags] "<task>"`. Pass arguments as literal CLI
arguments, never shell-evaluated; relay the output, its `ran on` line included, as-is, and a refusal without retrying.

See also: `~/.claude/commands/coworker.md` → See also (the subagent-workflows skill).
