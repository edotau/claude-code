---
description: "Send a task to the coworker (pi in code mode) on the Databricks arm, here or on --host <host>"
argument-hint: "[--host <host>] [--provider <p>] [--profile <p>] [--model <m>] [--effort <e>] [--fallback <mode>] [--codemode on|only] <task>"
disable-model-invocation: true
allowed-tools: Bash(claude-code agents label coworker *), Bash(claude-code agents brief --path pi), Read, Write,
  Bash(claude-code agents ask --background --agent pi *),
  Bash(claude-code agents ask --agent pi *), Bash(claude-code agents result -f *)
---

# /coworker

```text
/coworker [--host <host>] [--provider <p>] [--profile <p>] [--model <m>] [--effort <e>] [--fallback <mode>] [--codemode on|only] <task>

  --host <host>         run the whole ask on that machine (ssh alias, tailnet name or auto)
  --provider <p>        lead on this provider's signed-in profile; omitted: databricks (the least busy workspace)
  --profile <p>         the profile to lead on (dev, qa, prod, a login); omitted: the provider's pick
  --model <m>           alias, version, wire id or auto; omitted: the pin, else the tier pick
  --effort <e>          minimal|low|medium|high|xhigh|max
  --fallback <mode>     none|pin|agents|all; none keeps a typed model and route fixed
  --codemode on|only    on (default): pi in code mode, rtk off; only: every tool call runs as a script
```

Launcher only, a plain ask with no relay subagent (`--carry` refuses pi); the run is typed `coworker` and its receipt
reads `coworker via pi`. Empty `$ARGUMENTS` or `help`: print the block above and stop. Otherwise:

1. Take the leading flags in the block above off `$ARGUMENTS` (any order, each optional); the rest is the task.
   Add `--provider databricks --profile <p>` when neither `--provider` nor `--profile` was given, and `--codemode on` when
   `--codemode` was not.
2. Run `claude-code agents brief --path pi`, Read the path it prints, then Write the task verbatim to it.
3. Run `claude-code agents label coworker [--model <m> from step 1] --task <that path>` (prints `<label>:`; empty
   output means no prefix).
4. Run ONE Bash call, description `<label>: <first ~8 words of the task>`:
   `claude-code agents ask --background --agent pi [flags from step 1] --tools --autonomous - < <that path>`.
   With `--host`, drop `--background`, add `--host <host>`, and relay the foreground result directly; skip step 5.
   Allow the Bash call to wait for completion.
5. For a local ask, read the id from its `job <id> started` line, then wait with ONE
   `claude-code agents result -f <id>`.

Relay the report with its `ran on` line; never do the task yourself. Pi's bash is whole under the guard unless a file
deny applies (`rules/harness/agent.md` → pi). Commands: `~/.claude/rules/workflows/agents.md` → Slash commands.
