---
description: "Run an agent on the Databricks arm: Databricks first, first-party login as the fallback"
argument-hint: "<claude|codex|copilot|gemini|opencode> [profile=<p>] [model=<m>] [fallback=<f>] [effort=<e>] [tools=on|off|read-only] [route=<r>] [background=on] [host=<h>] <task>"
allowed-tools: Bash(claude-code agents ask --agent *)
---

# /coworker

```text
/coworker <agent> [key=value …] <task>        agent: claude|codex|copilot|gemini|opencode
/coworker:<agent> [key=value …] <task>        codex|copilot|gemini|opencode (commands/coworker/<agent>.md)
/coworker:<agent> [profile=<p>] [model=<m>] <verb> [args…]   → claude-code <agent> <verb> … (not routed)
  verbs: resume review explore transfer status tail result cancel setup catalog profile on off
  /coworker:codex adversarial [--base <b>|--commit <sha>|--pr] [focus]   adversarial diff review, saved as a report

  profile=<p>        --route databricks:<p>; omitted: --route databricks (the session's Databricks profile, or refused)
  model=<m>          --model <m>; omitted: the route's pick
  fallback=<f>       --fallback none|agents|all; omitted: all
  effort=<e>         --effort low|medium|high|xhigh|max; omitted: the agent's default
  tools=on|read-only --tools | --read-only; omitted or tools=off: no tools (answer only)
  route=<r>          --route default|first-party: leave the Databricks arm (profile= then means --profile <p>)
  background=on      --background: detach, print a job id (status | tail <id> | result <id>)
  host=<h>           --remote <h>: run on that ssh host; the route rides along, credentials never leave it
                     (its own vault resolves <p>; the same Databricks profile name must exist here too)

/coworker codex review internal/gateway
  → claude-code agents ask --agent codex --route databricks "review internal/gateway"
/coworker:copilot profile=prod model=databricks-claude-sonnet-5-5 fix the flaky watchdog test
  → claude-code agents ask --agent copilot --route databricks:prod --model databricks-claude-sonnet-5-5 "fix the …"
/coworker:gemini host=wsl profile=dev tools=read-only scan internal/memory for dead exports
  → claude-code agents ask --agent gemini --route databricks:dev --read-only --remote wsl "scan internal/memory …"
```

The route runs the agent on that Databricks workspace first, then first-party logins as the ordered fallback; every
hop is isolated. Routing, isolation and fallback are the binary's (`claude-code agents ask -h`).

Launcher only. Empty `$ARGUMENTS`, `help`, or a first word that is not one of the five agents: print the block above
verbatim and stop. Otherwise the first word is `<agent>`; turn each leading `key=value` above into its flag (an unknown
key is task text), and run the ONE call
`claude-code agents ask --agent <agent> --route databricks[:<p>] [flags] "<task>"`.
Pass arguments as literal CLI arguments, never shell-evaluated. Relay the output, its `ran on` line included, as-is; a
refusal (no Databricks profile, a flag outside the route) errors — relay that, don't retry.

## See also

- `~/.claude/skills/subagent-workflows/SKILL.md`: when to use `/coworker` vs an Agent-tool leaf, briefs, the handoff
  contract and how to verify the result; `references/agent-verbs.md` (ask/status/tail verbs),
  `references/cost-routing.md` (which agent for which task).
- `/coworker` runs one agent; multi-stage work is a saved workflow (see `skills/subagent-workflows`).
