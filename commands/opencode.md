---
description: "Send a task to the opencode agent on its own pin, here or on --host <host>"
argument-hint: "[--host <host>] [--provider <p>] [--profile <p>] [--model <m>] [--effort <e>] [--fallback <mode>] <task>"
disable-model-invocation: true
allowed-tools: Bash(claude-code agents label opencode *), Bash(claude-code agents brief --path opencode), Read, Write,
  Bash(claude-code agents ask --carry --background --overlay '{"agent":"opencode"*),
  Bash(claude-code agents ask --carry --background --overlay {"agent":"opencode"*),
  Bash(claude-code agents ask --overlay '{"agent":"opencode"*),
  Bash(claude-code agents ask --overlay {"agent":"opencode"*), Bash(claude-code agents result -f *)
---

# /opencode

```text
/opencode [--host <host>] [--provider <p>] [--profile <p>] [--model <m>] [--effort <e>] [--fallback <mode>] <task>

  --host <host>      run the whole ask on that machine (ssh alias, tailnet name or auto)
  --provider <p>     lead on this provider's signed-in profile (databricks picks the least busy workspace)
  --profile <p>      the profile to lead on (dev, qa, prod, a login); omitted: the agent's pin
  --model <m>        alias, version, wire id or auto; omitted: the pin, else the tier pick
  --effort <e>       minimal|low|medium|high|xhigh|max
  --fallback <mode>  none|pin|agents|all; none keeps a typed model and route fixed
```

Launcher only, carried with no relay subagent. Empty `$ARGUMENTS` or `help`: print the block above and stop.
Otherwise:

1. Take the leading flags in the block above off `$ARGUMENTS` (any order, each optional); the rest is the task.
2. Run `claude-code agents brief --path opencode`, Read the path it prints, then Write the task verbatim to it.
3. Run `claude-code agents label opencode [--model <m> from step 1] --task <that path>` (prints `<label>:`; empty
   output means no prefix).
4. Run ONE Bash call, description `<label>: <first ~8 words of the task>`:

   ```sh
   claude-code agents ask --carry --background --overlay '{"agent":"opencode","tools":true}' - < <that path>
   ```

   Add the step 1 flags as keys of that one object, in the order agent, provider, profile, model, then the rest
   alphabetically (`--host` becomes `"host"`, `--fallback` `"fallback"`, and so on). With `--host`, drop both
   `--carry` and `--background`, add `"host":"<host>"` to the overlay, and relay the foreground result directly; skip
   step 5. For provider alone add `"profile":"auto"`; for a registry profile alone add its provider. Keep
   `--profile session` as a flag after `--overlay` when no provider is given: its session-lead semantics differ.
   Allow the Bash call to wait.
5. For a local ask, read the id from its `job <id> started` line, then wait with ONE
   `claude-code agents result -f <id>`.

Relay the leaf's report with its `ran on` line; never do the task yourself. Commands:
`~/.claude/rules/harness/agent.md` → Slash commands.
