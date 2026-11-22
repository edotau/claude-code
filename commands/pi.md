---
description: "Send a task to the pi agent on its own pin, here or on --host <host>"
argument-hint: "[--host <host>] [--profile <p>] [--model <m>] <task>"
disable-model-invocation: true
allowed-tools: Bash(claude-code agents label pi *), Bash(claude-code agents brief --path pi), Read, Write,
  Bash(claude-code agents ask --background --agent pi *),
  Bash(claude-code agents ask --agent pi *), Bash(claude-code agents result -f *)
---

# /pi

```text
/pi [--host <host>] [--profile <p>] [--model <m>] <task>
```

Launcher only, a plain ask with no relay subagent (`--carry` refuses pi). Empty `$ARGUMENTS` or `help`: print the
block above and stop. Otherwise:

1. Take the leading `--host <host>`, `--profile <p>` and `--model <m>` off `$ARGUMENTS`; the rest is the task.
2. Run `claude-code agents brief --path pi`, Read the path it prints, then Write the task verbatim to it.
3. Run `claude-code agents label pi [--model <m> from step 1] --task <that path>` (prints `<label>:`; empty
   output means no prefix).
4. Run ONE Bash call, description `<label>: <first ~8 words of the task>`:
   `claude-code agents ask --background --agent pi [flags from step 1] --tools --autonomous - < <that path>`.
   With `--host`, drop `--background`, add `--host <host>`, and relay the foreground result directly; skip step 5.
   Allow the Bash call to wait for completion.
5. For a local ask, read the id from its `job <id> started` line, then wait with ONE
   `claude-code agents result -f <id>`.

Relay the report with its `ran on` line; never do the task yourself. Pi's bash is whole under the guard unless a file
deny applies (`rules/harness/agent.md` → pi). Commands: `~/.claude/rules/workflows/agents.md` → Slash commands.
