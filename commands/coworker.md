---
description: "Send a task to the coworker (pi in code mode) on the Databricks arm, here or on --host <host>"
argument-hint: "[--host <host>] [--provider <p>] [--profile <p>] [--model <m>] [--effort <e>] [--fallback <mode>] [--codemode on|only] <task>"
disable-model-invocation: true
allowed-tools: Bash(claude-code agents label coworker *), Bash(claude-code agents brief --path pi), Read, Write,
  Bash(claude-code agents ask --background --overlay '{"agent":"pi"*),
  Bash(claude-code agents ask --background --overlay {"agent":"pi"*),
  Bash(claude-code agents ask --overlay '{"agent":"pi"*),
  Bash(claude-code agents ask --overlay {"agent":"pi"*), Bash(claude-code agents result -f *)
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
reads `coworker`. Empty `$ARGUMENTS` or `help`: print the block above and stop. Otherwise:

1. Take the leading flags in the block above off `$ARGUMENTS` (any order, each optional); the rest is the task.
   When neither `--provider` nor `--profile` was given, add `"provider":"databricks","profile":"dev|qa|prod"` (pick
   one), or `"provider":"databricks","profile":"auto"` for the least-busy pick (`--profile auto` is refused). Add
   `"codemode":"on"` when `--codemode` was not given.
2. Run `claude-code agents brief --path pi`, Read the path it prints, then Write the task verbatim to it.
3. Run `claude-code agents label coworker [--model <m> from step 1] --task <that path>` (prints `<label>:`; empty
   output means no prefix).
4. Run ONE Bash call, description `<label>: <first ~8 words of the task>`:

   ```sh
   claude-code agents ask --background --overlay '{"agent":"pi","provider":"databricks","profile":"auto","autonomous":true,"codemode":"on","tools":true}' - < <that path>
   ```

   Replace the example's route and code mode with the selections from step 1; add the remaining flags as keys of that
   one object, in the order agent, provider, profile, model, then the rest
   alphabetically (`--host` becomes `"host"`). With `--host`, drop `--background`, add `"host":"<host>"` to the
   overlay, and relay the foreground result directly; skip step 5. For provider alone add `"profile":"auto"`; for a
   registry profile alone add its provider. For `--profile session` without a provider, remove both example route keys
   and pass `--profile session` as a flag after `--overlay`.
   Allow the Bash call to wait for completion.
5. For a local ask, read the id from its `job <id> started` line, then wait with ONE
   `claude-code agents result -f <id>`.

Relay the report with its `ran on` line; never do the task yourself. Pi's bash is whole under the guard unless a file
deny applies (`rules/harness/agent.md` → pi). Commands: `~/.claude/rules/workflows/agents.md` → Slash commands.
