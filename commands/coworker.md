---
description: "Send a task to an agent here, with its Databricks requests leaving through the other machine"
argument-hint: "--agent <codex|copilot|gemini|opencode|claude> [--remote <host>] [--profile <p>] [--model <m>] <task>"
disable-model-invocation: true
allowed-tools: Bash(claude-code agents label *), Agent
---

# /coworker

```text
/coworker --agent <codex|copilot|gemini|opencode|claude> [--remote <host>] [--profile <p>] [--model <m>] <task>

  --remote <host>   the machine the Databricks requests leave through; omitted: --remote auto
                    (HARNESS_REMOTE_HOST, else the one other tailnet machine that answers)
  --profile <p>     the Databricks workspace (--route databricks:<p>); omitted: --route databricks:auto (the
                    workspace with the fewest live dispatches)
  --model <m>       the model; omitted: the route's pick
```

The `/<agent>` dispatch with one difference: the agent still runs HERE, on this checkout, but on the Databricks arm
with its Databricks requests sent through the other machine. Not a new agent or type. Launcher only. Empty
`$ARGUMENTS`, `help`, or no `--agent` naming one of the five: print the block above and stop. Otherwise:

1. Take the leading `--agent <type>`, `--remote <host>`, `--profile <p>` and `--model <m>` off `$ARGUMENTS`; the rest
   is the task.
2. Run `claude-code agents label <type>`: it prints the label, `<label>:`.
3. Make ONE Agent tool call: `subagent_type: <type>`, description `<label>: <first ~8 words of the task>`, and a
   prompt whose first line is
   `Run: claude-code --remote <host|auto> agents ask --agent <type> --route databricks:<p|auto> [--model <m>] --tools
   --autonomous` (one line; codex has no autonomous mode and drops the flag with a note),
   then a blank line, then the task verbatim.

Relay the leaf's report, its `ran on` line included; never do the task yourself. A refusal (no machine, no workspace,
something missing on the target) is the answer: relay it, don't retry. All five commands:
`~/.claude/rules/workflows/agents.md` → Slash commands.
