---
description: "Send a task to an agent here under one overlay; Databricks requests go via the other machine by default"
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

Launcher only (a shortcut for `agents ask --overlay`). Empty `$ARGUMENTS`, `help`, or no `--agent` naming one of the
five: print the block above and stop. Otherwise:

1. Take the leading `--agent`, `--overlay` and `--model` off `$ARGUMENTS`; the rest is the task. A given overlay is
   passed through unchanged; the default overlay is used only when none is given.
2. Run `claude-code agents label <type>` (prints `<label>:`).
3. Make ONE Agent tool call: `subagent_type: <type>`, description `<label>: <first ~8 words of the task>`, and a
   prompt whose first line is
   `Run: claude-code agents ask --agent <type> --overlay '<overlay JSON or file path>'
   [--model <m>] --tools --autonomous` (one line; codex drops --autonomous with a note), then a blank line, then the
   task verbatim.

Relay the leaf's report with its `ran on` line; never do the task yourself. A refusal is the answer: relay it.
