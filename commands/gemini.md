---
description: "Send a task to the gemini agent on its own pin, here or on --host <host>"
argument-hint: "[--host <host>] [--profile <p>] [--model <m>] <task>"
disable-model-invocation: true
allowed-tools: Bash(claude-code agents label gemini *), Bash(claude-code agents brief --path gemini), Read, Write,
  Bash(claude-code agents ask --spec *), Bash(claude-code agents result -f *)
---

# /gemini

```text
/gemini [--host <host>] [--profile <p>] [--model <m>] <task>
```

Launcher only, carried with no relay subagent. Empty `$ARGUMENTS` or `help`: print the block above and stop.
Otherwise:

1. Take the leading `--host <host>`, `--profile <p>` and `--model <m>` off `$ARGUMENTS`; the rest is the task.
2. Run `claude-code agents brief --path gemini`, Read the path it prints, then Write the task verbatim to it.
3. Run `claude-code agents label gemini [--model <m> from step 1] --task <that path>` (prints `<label>:`; empty
   output means no prefix).
4. Write the spec, one JSON object, to `<that path>.spec.json`: `{"agent":"gemini","tools":true,"carry":true,
   "background":true,"prompt":"<task, JSON-escaped>"}` plus `"profile"` and `"model"` when given. With `--host`,
   add `"host":"<host>"` and leave out `carry` and `background`.
5. Run ONE Bash call, description `<label>: <first ~8 words of the task>`:
   `claude-code agents ask --spec <that path>.spec.json`. With `--host`, relay the foreground result directly and
   skip step 6; allow the call to wait for completion.
6. For a local ask, read the id from its `job <id> started` line, then wait with ONE
   `claude-code agents result -f <id>`.

Relay the result with its `ran on` line; never do the task yourself. Commands:
`~/.claude/rules/workflows/agents.md` → Slash commands.
