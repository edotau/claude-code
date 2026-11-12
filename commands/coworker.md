---
description: "Send a task to an agent here under one overlay; Databricks requests leave via the other machine by default"
argument-hint: "--agent <codex|copilot|gemini|opencode|claude> [--overlay <json|file>] [--model <m>] <task>"
disable-model-invocation: true
allowed-tools: Bash(claude-code agents label *), Agent
---

# /coworker

```text
/coworker --agent <codex|copilot|gemini|opencode|claude> [--overlay <json|file>] [--model <m>] <task>

  --overlay <json|file>  what `agents ask --overlay` takes (inline JSON, a file path, or `-`); omitted:
                         {"provider":"databricks","profile":"auto","remote":"auto"}
  --model <m>            the model; omitted: the pick
```

Overlay keys: `rules/harness/agent.md` → Dispatch behavior.

A shortcut for `agents ask --overlay`. Launcher only. Empty `$ARGUMENTS`, `help`, or no `--agent` naming one of the
five: print the block above and stop. Otherwise:

1. Take the leading `--agent`, `--overlay` and `--model` off `$ARGUMENTS`; the rest is the task. A given overlay is
   passed through unchanged; the default overlay is used only when none is given.
2. Run `claude-code agents label <type>`: it prints the label, `<label>:`.
3. Make ONE Agent tool call: `subagent_type: <type>`, description `<label>: <first ~8 words of the task>`, and a
   prompt whose first line is
   `Run: claude-code agents ask --agent <type> --overlay '<overlay JSON or file path>'
   [--model <m>] --tools --autonomous` (one line; codex drops --autonomous with a note), then a blank line, then the
   task verbatim.

Relay the leaf's report, its `ran on` line included; never do the task yourself. A refusal is the answer: relay it.
