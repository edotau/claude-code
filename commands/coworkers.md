---
description: "Default pool for external-agent work and model discovery: run tasks on copilot, codex, opencode, gemini or claude (single, compare, relay); copilot/opencode can rotate through discovered models (deepseek, kimi, glm…)."
argument-hint: "[agents=a,b] [model=m|byok] [profile=p] [single|compare|relay] task|…"
allowed-tools: Bash, Read, Grep, Glob, Agent
---

Launcher only. The leaf's procedure and report live in `agents/coworker.md`; the dispatch shape and the
Handoff contract in `skills/subagent-workflows/SKILL.md`.

Empty `$ARGUMENTS` or `help`: print the Usage block below verbatim and stop.

From `$ARGUMENTS`, strip an optional leading mode word (`single`/`compare`/`relay`; default `single`) and
an optional `agents=` list (default: copilot whenever available, then codex; task-class picks per
`skills/subagent-workflows/references/cost-routing.md`); split the rest on `|` into the task list.

Dispatch each task as one `coworker` leaf (`subagent_type: "coworker"`), all in ONE message in the background (SKILL.md
→ Concurrency: ≤2 while another session shares the checkout),
with a brief per the Handoff contract that names `Agent:` (one line), `Mode:` (`single|compare|relay`;
absent with several names = relay), `Owns:` and `Verify:`. A feature or
bug-fix task also gets a `copilot` test agent in the SAME message. Edits follow
`rules/standards/worktree-git.md` (shared tree; a worktree only with the user's yes). Relay one consolidated report: per
task, the agent rows (agent · model · job · fallback · verified), the verified answer or change, and for `compare`
where the agents agreed and differed.

A `profile=<name>` argument adds `Profile: <name>` to each brief: the leaf runs `ask --profile`,
host and auth from the vault.
A `model=<m>` argument puts `--model <m>` on each brief's `ask` request line (an alias, wire id, discovered
model, or `byok`).

Pick and fallback: `skills/subagent-workflows/references/cost-routing.md`.

## Usage

```text
/coworkers [agents=<a,b>] [model=<m>|byok] [profile=<p>] [single|compare|relay] <task> [| <task> …]

  agents=   copilot (first when available) · codex · opencode · gemini · claude
            several names = relay unless a mode word is given
  model=    alias (opus|sonnet|haiku), wire id, or a discovered model (kimi-k3, deepseek-v4-pro, glm-5.3…);
            byok = the router picks a rare model from the discovered pool (copilot/opencode only)
  profile=  vault profile that bills the run (dev, prod, …); omitted = the session profile
  mode      single (default) · compare (same task, side by side) · relay (each answer feeds the next)

Model discovery: copilot and opencode take flexible endpoints. On a Databricks profile they serve its
endpoints; without one, route through the discovered catalogs (claude-code opencode catalog,
claude-code copilot catalog; refresh with /discover-models).

Examples:
  /coworkers review internal/proxy for races                           copilot, single
  /coworkers agents=copilot,codex compare is this diff safe to ship?   both answers side by side
  /coworkers agents=gemini,codex scan internal/memory for dead exports gemini scans, codex refutes
  /coworkers agents=opencode model=kimi-k3 summarize docs/             one discovered model
  /coworkers agents=copilot,opencode model=byok compare refactor X     router-picked rare models
  /coworkers profile=prod model=opus fix the flaky watchdog test        billed to prod
```
