---
description: "Send a task to an agent here under one overlay; Databricks requests go via the other machine by default"
argument-hint: "--agent <codex|copilot|gemini|opencode|claude|pi> [--overlay <json|file>] [--model <m>] <task>"
disable-model-invocation: true
allowed-tools: Bash(claude-code agents label *), Bash(claude-code agents brief --path *), Read, Write,
  Bash(claude-code agents ask --carry --background --agent *), Bash(claude-code agents ask --background --agent *),
  Bash(claude-code agents result -f *)
---

# /coworker

```text
/coworker --agent <codex|copilot|gemini|opencode|claude|pi> [--overlay <json|file>] [--model <m>] <task>

  --overlay <json|file>  what `agents ask --overlay` takes (inline JSON or a file path, not `-`); omitted:
                         {"provider":"databricks","profile":"auto","remote":"auto"}
  --model <m>            the model; omitted: the pick
```

Overlay keys: `rules/harness/agent.md` → Dispatch behavior.

Launcher only (a shortcut for `agents ask --overlay`), run by this session with no relay subagent. Empty
`$ARGUMENTS`, `help`, or no `--agent` naming one of the six: print the block above and stop. Otherwise:

1. Take the leading `--agent`, `--overlay` and `--model` off `$ARGUMENTS`; the rest is the task. A given overlay is
   passed through unchanged; the default overlay is used only when none is given.
2. Run `claude-code agents brief --path <type>`, Read the path it prints, then Write the task verbatim to it.
3. Run `claude-code agents label <type> [--model <m> from step 1] --task <that path>` (prints `<label>:`; empty
   output means no prefix).
4. Run ONE Bash call, description `<label>: <first ~8 words of the task>`:
   `claude-code agents ask --carry --background --agent <type> --overlay '<overlay JSON or file path>' [--model <m>]
   --tools --autonomous - < <that path>` (codex drops --autonomous with a note; claude and pi drop `--carry`, which
   refuses them).
5. Read the id from its `job <id> started` line, then wait with ONE `claude-code agents result -f <id>`.

Relay the report with its `ran on` line; never do the task yourself. A refusal is the answer: relay it.
