---
description: "Send a task to the opencode agent on its own pin, here or on --host <host>"
argument-hint: "[--host <host>] [--profile <p>] [--model <m>] <task>"
disable-model-invocation: true
allowed-tools: Bash(claude-code agents label opencode), Agent
---

# /opencode

```text
/opencode [--host <host>] [--profile <p>] [--model <m>] <task>
```

Launcher only. Empty `$ARGUMENTS` or `help`: print the block above and stop. Otherwise:

1. Take the leading `--host <host>`, `--profile <p>` and `--model <m>` off `$ARGUMENTS`; the rest is the task.
2. Run `claude-code agents label opencode` (prints `<label>:`).
3. Make ONE Agent tool call: `subagent_type: opencode`, description `<label>: <first ~8 words of the task>`, and a
   prompt whose first line is
   `Run: claude-code agents ask --agent opencode [the flags from step 1] --tools --autonomous`,
   then a blank line, then the task verbatim.

Relay the leaf's report with its `ran on` line; never do the task yourself. Commands:
`~/.claude/rules/workflows/agents.md` → Slash commands.
