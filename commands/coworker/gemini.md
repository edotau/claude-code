---
description: "Run gemini: a task on the Databricks arm (Databricks first, first-party login as the fallback), or one of its verbs"
argument-hint: "[key=value …] <task> | [profile=<p>] [model=<m>] <verb> [args…]"
allowed-tools: Bash(claude-code agents ask --agent gemini *), Bash(claude-code gemini *)
disable-model-invocation: true
---

# /coworker:gemini

Launcher only. Arguments, defaults and examples: `~/.claude/commands/coworker.md`; verbs and their flags:
`claude-code gemini -h`; the agent's role and report contract: `~/.claude/agents/gemini.md`.

1. Empty `$ARGUMENTS` or `help`: print the usage block of `~/.claude/commands/coworker.md` and stop.
2. Strip the leading `key=value` arguments. First remaining word is a verb → run
   `claude-code gemini <verb> [--profile <p>] [--model <m>] <rest>` (only `profile=` and `model=` apply to a verb).
   Verbs: `resume review explore transfer status tail result cancel setup catalog profile on off`.
3. Anything else is a task (a leading `ask` is dropped) → turn each `key=value` into its flag and run the ONE call
   `claude-code agents ask --agent gemini --route databricks[:<p>] [flags] "<task>"`; `route=<r>` replaces the route.

Pass arguments as literal CLI arguments, never shell-evaluated. Long tasks: `background=on`, then relay the job id
(`/coworker:gemini status`, `tail <id>`, `result <id>`). Relay the output, its `ran on` line
included, as-is; a refusal or a verb that rejects a flag errors — relay that, don't retry.

See also: `~/.claude/commands/coworker.md` → See also (the subagent-workflows skill).
