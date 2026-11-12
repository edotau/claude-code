# Agents and Models: What Runs Where

Role selection and dispatch: `../SKILL.md` → Leaf types. Shell flags and lifecycle:
[agent-verbs.md](agent-verbs.md). Routing and failure policy: [cost-routing.md](cost-routing.md).

## Live sources

- `claude-code agents list --format json` prints registered CLI agents and capabilities; it is not the Agent roster.
- `claude-code <agent> profile` shows that machine's current pin; never copy it to another machine.
- `claude-code gateway --agent <agent>` previews routing. `claude-code <agent> catalog` lists the current menu;
  OpenCode uses `catalog list` and can query a requested profile. Do not infer availability from an old benchmark.
- Provider aliases live in `internal/providers/defaults.json` plus the vault overlay; model alias resolution is
  under `internal/models`. The router's table records concrete model/account prices.

## Execution surfaces

| Agent | CLI | Instructions |
| --- | --- | --- |
| `claude` | Claude Code | `~/.claude/CLAUDE.md` and native scoped rules |
| `codex` | OpenAI Codex | `~/.codex/AGENTS.md`, rendered by sync |
| `copilot` | Copilot SDK for headless ask | rendered instructions plus SDK context |
| `gemini` | Antigravity (`agy`) | `~/.gemini/GEMINI.md`, rendered by sync |
| `opencode` | OpenCode | native `CLAUDE.md` plus launch overlay |

Projection details: `rules/harness/agent.md`. Hook applicability: `rules/harness/hooks.md`.
Unsupported flags appear in the result's `dropped`; read-only behavior varies by CLI. In particular codex/claude
read-only mode drops tools, whereas Gemini needs its read tools for packed context. Check capability definitions
in each agent's registration package when a task depends on a boundary.

## Per-call routes

`--route databricks:<profile>` selects the Databricks arm without changing the agent's persistent pin.
`--route databricks:auto` chooses the workspace with the fewest live dispatches. `--fallback none` refuses a missing
credential or unsupported route instead of changing agents. Other fallback policies must be explicit in the brief;
report the actual run receipt, not a planned model. `--remote <h>` keeps execution here and tunnels Databricks calls;
`--host <h>` executes on that host, where profiles resolve.

## Measurements

The audit's historical runs are preserved in the source handoff at
`memory/claude-code/handoffs/2026-11-11-prompt-audit/leg-D.result.md` and its sibling reports. Measurements describe
a particular machine, date and configuration; they do not establish current pins, workspace availability, model
accuracy or a required default agent. Verify a representative task before making a performance claim.
