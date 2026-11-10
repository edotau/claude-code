---
description: "Default pool for external-agent work and model discovery: run tasks on copilot, opencode or Databricks (single, compare, relay); copilot/opencode can rotate through discovered models (deepseek, kimi, glm…)."
argument-hint: "[agents=copilot|opencode|databricks,…] [model=m|discovery] [profile=p] [single|compare|relay|team] task|…"
allowed-tools: Bash, Read, Grep, Glob, Agent
disable-model-invocation: true
---

Launcher only. The leaf's procedure and report live in `~/.claude/agents/coworker.md`; the dispatch shape and the
Handoff contract in `~/.claude/skills/subagent-workflows/SKILL.md`.

Empty `$ARGUMENTS` or `help`: print the Usage block below verbatim and stop.

From `$ARGUMENTS`, strip an optional leading mode word (`single`/`compare`/`relay`/`team`; default `single`) and
an optional `agents=` list (default: copilot whenever available; task-class picks per
`~/.claude/skills/subagent-workflows/references/cost-routing.md`); split the rest on `|` into the task list.

Dispatch each task as one `coworker:<a>` leaf (`subagent_type: "coworker:copilot|opencode|databricks"`; compare or
relay across several = the bare `coworker` leaf with an `Agent:` list), all in ONE message in the background
(parallel only on disjoint files; SKILL.md → Concurrency), with a brief per the Handoff contract that names
`Agent:` (one line), `Mode:` (`single|compare|relay`;
absent with several names = relay), `Owns:` and `Verify:`. A feature or
bug-fix task also gets a `copilot` test agent in the SAME message. Edits follow
`rules/standards/worktree-git.md` (shared tree; a worktree only with the user's yes). Relay one consolidated report: per
task, the agent rows (agent · model · job · fallback · verified), the verified answer or change, and for `compare`
where the agents agreed and differed.

A `profile=<name>` argument adds `Profile: <name>` to each brief: the leaf runs `agents ask --profile`,
host and auth from the vault.
A `model=<m>` argument puts `--model <m>` on each brief's `ask` request line (an alias, wire id, discovered
model, or `discovery`).

`databricks` forces the brief's agent onto a Databricks profile: an `agent=<a>` sub-token sets it (default claude), and
every brief carries `Profile:` (the `profile=` value, else the vault's Databricks profile) so the leaf passes `--profile`.

`team` mode (`/coworker team <goal>`): a `gemini` leaf scans (read-only, cites file:line) →
a `codex` leaf reviews the list (KEEP/CHANGE/DROP) → STOP and show the reviewed list for the user's approval →
approved items go to `coworker:opencode` leaves (shared tree; worktree only with the user's yes) ‖ a
`copilot` test agent (feature/bug fix only) when code changes. `agents=` overrides the three roles in order.
Watch each dispatch with `claude-code <agent> tail -f`.

Pick and fallback: `~/.claude/skills/subagent-workflows/references/cost-routing.md`.

# Usage

```text
/coworker [agents=<a,b>] [model=<m>|discovery] [profile=<p>] [single|compare|relay|team] <task> [| <task> …]

  agents=   copilot (first when available) · opencode · databricks (agent=<a> picks its agent, default claude)
            several names = relay unless a mode word is given (bare coworker leaf)
  model=    alias (opus|sonnet|haiku|fable), wire id, or a discovered model (kimi-k3, deepseek-v4-pro, glm-5.3…);
            discovery = the router picks a rare model from the discovered pool (copilot/opencode only)
  profile=  vault profile that bills the run (dev, prod, …); omitted = the session profile
  mode      single (default) · compare (same task, side by side) · relay (each answer feeds the next) ·
            team (gemini scan → codex review → your approval → opencode edits)

Model discovery: copilot and opencode take flexible endpoints. model=discovery needs a Databricks
profile; without one it falls back to the alias (Claude Sonnet on copilot), so give copilot
model=gpt-6.1-sol (its default is Claude Sonnet); opencode defaults to opencode-go (claude-code <agent> catalog).

Examples:
  /coworker review internal/gateway for races                         copilot, single
  /coworker agents=copilot,opencode compare is this diff safe to ship? both answers side by side
  /coworker agents=databricks agent=codex review internal/gateway       codex on a Databricks profile
  /coworker agents=opencode model=kimi-k3 summarize docs/             one discovered model
  /coworker agents=copilot,opencode model=discovery compare refactor X     router-picked rare models
  /coworker profile=prod model=opus fix the flaky watchdog test        billed to prod
  /coworker team tighten error handling in internal/usage               scan, review, approve, edit
```
