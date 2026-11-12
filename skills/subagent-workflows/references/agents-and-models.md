# Agents and Models: What Runs Where

Role selection and dispatch: `../SKILL.md` → Leaf types. Shell flags and lifecycle:
[agent-verbs.md](agent-verbs.md). Routing, aliases and failure policy: [cost-routing.md](cost-routing.md).

## Live sources

- `claude-code agents list --format json` prints registered CLI agents and capabilities; it is not the Agent roster.
- `claude-code <agent> profile` shows that machine's current pin; never copy it to another machine.
- `claude-code gateway --agent <agent>` previews routing. `claude-code <agent> catalog` lists the current menu;
  OpenCode uses `catalog list` and can query a requested profile. Do not infer availability from an old benchmark.

## Execution surfaces

| Agent | CLI | Instructions |
| --- | --- | --- |
| `claude` | Claude Code | `~/.claude/CLAUDE.md` and native scoped rules |
| `codex` | OpenAI Codex | `~/.codex/AGENTS.md`, rendered by sync |
| `copilot` | Copilot SDK for headless ask | rendered instructions plus SDK context |
| `gemini` | Antigravity (`agy`) | `~/.gemini/GEMINI.md`, rendered by sync |
| `opencode` | OpenCode | native `CLAUDE.md` plus launch overlay |

Projection and per-agent read-only behavior: `rules/harness/agent.md`. Hook applicability: `rules/harness/hooks.md`.
Unsupported flags appear in the result's `dropped`; check capability definitions in each agent's registration
package when a task depends on a boundary.

## Per-call routes

`--route databricks:<profile>` selects the Databricks arm without changing the agent's persistent pin; `:auto`,
`--remote` and `--host`: `rules/harness/network.md`. Report the actual run receipt, not a planned model.

## Measurements

The audit's historical runs are preserved in the source handoff at
`memory/claude-code/handoffs/2026-11-11-prompt-audit/leg-D.result.md` and its sibling reports. Measurements describe
a particular machine, date and configuration; they do not establish current pins, workspace availability, model
accuracy or a required default agent. Verify a representative task before making a performance claim.
