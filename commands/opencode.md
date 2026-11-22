---
description: "Send a task to the opencode agent on its own pin, here or on --host <host>"
argument-hint: "[--host <host>] [--profile <p>] [--model <m>] <task>"
disable-model-invocation: true
allowed-tools: Bash(claude-code agents label opencode *), Bash(claude-code agents brief --path opencode), Read, Write,
  Bash(claude-code agents ask --carry --background --agent opencode *),
  Bash(claude-code agents ask --agent opencode *), Bash(claude-code agents result -f *)
---

# /opencode

```text
/opencode [--host <host>] [--profile <p>] [--model <m>] <task>
```

Launcher only, carried with no relay subagent. Empty `$ARGUMENTS` or `help`: print the block above and stop.
Otherwise:

1. Take the leading `--host <host>`, `--profile <p>` and `--model <m>` off `$ARGUMENTS`; the rest is the task.
2. Run `claude-code agents brief --path opencode`, Read the path it prints, then Write the task verbatim to it.
3. Run `claude-code agents label opencode [--model <m> from step 1] --task <that path>` (prints `<label>:`; empty
   output means no prefix).
4. Run ONE Bash call, description `<label>: <first ~8 words of the task>`:
   `claude-code agents ask --carry --background --agent opencode [flags from step 1] --tools - < <that path>`.
   With `--host`, drop both `--carry` and `--background`, add `--host <host>`, and relay the foreground result
   directly; skip step 5. Allow the Bash call to wait for completion.
5. For a local ask, read the id from its `job <id> started` line, then wait with ONE
   `claude-code agents result -f <id>`.

Relay the leaf's report with its `ran on` line; never do the task yourself. Commands:
`~/.claude/rules/workflows/agents.md` → Slash commands.
