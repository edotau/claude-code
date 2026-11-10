---
description: "Dispatch tasks to external agents (codex, copilot, gemini, opencode, claude): single, compare, relay or team, optionally on a Databricks profile or discovered model"
argument-hint: "[agents=<a,b>] [model=<m>|discovery] [profile=<p>] [single|compare|relay|team] <task> [| <task> …]"
allowed-tools: Bash, Read, Grep, Glob, Agent
disable-model-invocation: true
---

# /coworker

Launcher only. Leaf roles: `~/.claude/agents/<agent>.md`; brief fields, dispatch shape and fan-in:
`~/.claude/skills/subagent-workflows/SKILL.md`; agent pick and fallback:
`~/.claude/skills/subagent-workflows/references/cost-routing.md`.

Empty `$ARGUMENTS` or `help`: print the Usage block below verbatim and stop.

1. Strip the options from `$ARGUMENTS`: `agents=` (comma list of `codex copilot gemini opencode claude databricks`;
   default `copilot`, else the cost-routing pick for the task), `model=<m>`, `profile=<p>`, and one mode word
   (`single` default; several agents with no mode word = `relay`). Split the rest on `|` into tasks.
2. `databricks` in `agents=` means "run on a Databricks profile": every brief gets `Profile: <profile= value, else the
   vault's first Databricks profile>`, and `agent=<a>` beside it picks the agent (default `claude`).
3. Dispatch, all in ONE message with `run_in_background`, one `Agent` call per task × agent:
   `subagent_type` = the agent (`codex`, `copilot`, `gemini`, `opencode`, `claude`). Each brief names `Agent:`,
   `Mode:`, `Owns:` (exact files; parallel leaves never share one), `Verify:`, plus `Profile: <p>` when given and
   `--model <m>` on its `ask` line when `model=` is set. A feature or bug-fix task also gets a `copilot` test agent
   in the same message. Edits follow `~/.claude/rules/standards/worktree-git.md` (shared tree; worktree only on the
   user's yes).
   - `single`: one leaf per task. `compare`: the same task to each agent, read-only unless the user said edit.
   - `relay`: run the agents in order, each brief carrying the previous answer.
   - `team <goal>`: `gemini` scans (read-only, file:line) → `codex` reviews the list (KEEP/CHANGE/DROP) → STOP and show
     the reviewed list for approval → approved items to `opencode` leaves ‖ a `copilot` test agent. `agents=` overrides
     the three roles in order.
4. Fan-in: treat each report as a claim (check `ran on`, `git diff --stat`, the `Verify:` command). Relay one report:
   per task a row `agent · model · fallback · verified`, the answer or change, and for `compare` where the agents
   agreed and differed. A leaf with no `ran on` line or a fallback is reported as such, never as its agent's work.

## Usage

```text
/coworker [agents=<a,b>] [model=<m>|discovery] [profile=<p>] [single|compare|relay|team] <task> [| <task> …]

  agents=   codex · copilot · gemini · opencode · claude · databricks (agent=<a> picks its agent, default claude)
            default copilot; several names with no mode word = relay
  model=    alias (opus|sonnet|haiku|fable), wire id, a discovered model (kimi-k3, deepseek-v4-pro…), or
            discovery (copilot/opencode only; needs a Databricks profile)
  profile=  vault profile that bills the run (dev, qa, prod…); omitted = each agent's pin
  mode      single (default) · compare (side by side) · relay (each answer feeds the next) ·
            team (gemini scan → codex review → your approval → opencode edits)
  | <task>  several tasks run as parallel leaves (disjoint files only)

Examples:
  /coworker agents=codex,gemini compare review internal/gateway for dead code
  /coworker profile=dev agents=opencode fix the flaky TestX in internal/router
  /coworker agents=databricks agent=codex profile=qa explain internal/router/pick.go
  /coworker team shrink internal/usage to its key features
```
