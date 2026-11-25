---
description: "Send a task to the gemini agent on its own pin, here or on --host <host>"
argument-hint: "[--host <host>] [--provider <p>] [--profile <p>] [--model <m>] [--effort <e>] [--fallback <mode>] <task>"
disable-model-invocation: true
allowed-tools: Bash(claude-code agents label gemini *), Bash(claude-code agents brief --path gemini), Read, Write,
  Bash(claude-code agents ask --agent gemini --spec *), Bash(claude-code agents result -f *)
---

# /gemini

```text
/gemini [--host <host>] [--provider <p>] [--profile <p>] [--model <m>] [--effort <e>] [--fallback <mode>] <task>

  --host <host>      run the whole ask on that machine (ssh alias, tailnet name or auto)
  --provider <p>     lead on this provider's signed-in profile (databricks picks the least busy workspace)
  --profile <p>      the profile to lead on (dev, qa, prod, a login); omitted: the agent's pin
  --model <m>        alias, version, wire id or auto; omitted: the pin, else the tier pick
  --effort <e>       low|medium|high|xhigh|max
  --fallback <mode>  none|pin|agents|all; none keeps a typed model and route fixed
```

Launcher only, carried with no relay subagent. Empty `$ARGUMENTS` or `help`: print the block above and stop.
Otherwise:

1. Take the leading flags in the block above off `$ARGUMENTS` (any order, each optional); the rest is the task.
2. Run `claude-code agents brief --path gemini`, Read the path it prints, then Write the task verbatim to it.
3. Run `claude-code agents label gemini [--model <m> from step 1] --task <that path>` (prints `<label>:`; empty
   output means no prefix).
4. Run `claude-code agents brief --path gemini` again and Write the spec there, one JSON object with no prompt
   (the task stays in the first file, any size): `{"tools":true,"carry":true,"background":true}`
   plus `"profile"` and `"model"` when given. With `--host`, add `"host":"<host>"` and leave out `carry` and
   `background`.
5. Run ONE Bash call, description `<label>: <first ~8 words of the task>`:
   `claude-code agents ask --agent gemini --spec <spec path> - < <task path>`. With `--host`, relay the foreground
   result directly and skip step 6; allow the call to wait for completion.
6. For a local ask, read the id from its `job <id> started` line, then wait with ONE
   `claude-code agents result -f <id>`.

Relay the result with its `ran on` line; never do the task yourself. Commands:
`~/.claude/rules/workflows/agents.md` → Slash commands.
