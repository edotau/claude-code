---
description: "Send a task to the gemini agent on this machine, on its own pin"
argument-hint: "[--remote <host>] [--profile <p>] [--model <m>] <task>"
disable-model-invocation: true
allowed-tools: Bash(claude-code agents label gemini), Agent
---

# /gemini

```text
/gemini [--remote <host>] [--profile <p>] [--model <m>] <task>
```

Launcher only. Empty `$ARGUMENTS` or `help`: print the block above and stop. Otherwise:

1. Take the leading `--remote <host>`, `--profile <p>` and `--model <m>` off `$ARGUMENTS`; the rest is the task.
2. Run `claude-code agents label gemini`: it prints the label, `<label>:`.
3. Make ONE Agent tool call: `subagent_type: gemini`, description `<label>: <first ~8 words of the task>`, and a
   prompt whose first line is `Run: claude-code agents ask --agent gemini [the flags from step 1] --tools`, then a
   blank line, then the task verbatim.

Relay the leaf's report, its `ran on` line included; never do the task yourself. All five commands:
`~/.claude/rules/workflows/agents.md` → Slash commands.
