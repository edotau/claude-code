---
description: "Run codex: a task on the Databricks arm (Databricks first, first-party login as the fallback), or one of its verbs"
argument-hint: "[key=value …] <task> | [profile=<p>] [model=<m>] <verb|adversarial> [args…]"
allowed-tools: Bash(claude-code agents ask --agent codex *), Bash(claude-code codex *), Bash(claude-code memory path), Bash(date +%F), Bash(mkdir -p *), Bash(tee *)
---

# /coworker:codex

Launcher only. Arguments, defaults and examples: `~/.claude/commands/coworker.md`; verbs and their flags:
`claude-code codex -h`; the agent's role and report contract: `~/.claude/agents/codex.md`.

1. Empty `$ARGUMENTS` or `help`: print the usage block of `~/.claude/commands/coworker.md` and stop.
2. Strip the leading `key=value` arguments. First remaining word is a verb → run
   `claude-code codex <verb> [--profile <p>] [--model <m>] <rest>` (only `profile=` and `model=` apply to a verb).
   Verbs: `resume review explore transfer status tail result cancel setup catalog profile on off`.
3. First word `adversarial` → `review --adversarial` over the rest (scope `--uncommitted` default, `--base <b>`,
   `--commit <sha>`, `--pr`; other words = focus), teed to a report; `<scope>` = `uncommitted`, `base-<b>`,
   `commit-<sha>` or `pr`:

   ```bash
   out="$(claude-code memory path)/reports/$(date +%F)-codex-review-<scope>.md"
   mkdir -p "${out%/*}"; claude-code codex review --adversarial <args> | tee "$out"
   ```

   `nothing in scope` (exit 1) → say so and stop. Relay the report path.

4. Anything else is a task (a leading `ask` is dropped) → turn each `key=value` into its flag and run the ONE call
   `claude-code agents ask --agent codex --route databricks[:<p>] [flags] "<task>"`; `route=<r>` replaces the route.

Pass arguments as literal CLI arguments, never shell-evaluated. Long tasks: `background=on`, then relay the job id
(`/coworker:codex status`, `tail <id>`, `result <id>`). Relay the output, its `ran on` line
included, as-is; a refusal or a verb that rejects a flag errors — relay that, don't retry.

See also: `~/.claude/commands/coworker.md` → See also (the subagent-workflows skill).
