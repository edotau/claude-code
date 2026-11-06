---
paths:
  - "agents/**"
  - "workflows/**"
  - "commands/**"
---
# Agent Authoring and Commands

## Commands

| To… | Run |
| --- | --- |
| Ask another agent (one shot) | `claude-code ask --agent <agent> "<task>"` (`--format json` = envelope) · `ask --log` · `ask --retry <id>` · inline agents: `ask --agents <json\|file>` (`rules/harness/agent.md` → Dispatch behavior) |
| Stream events, set env/system prompt, run remote, or hold a live claude session | `ask --format stream-json` · `--env K=V` · `--append-system-prompt` · `--host <h>` · `--live` + `agents send <id> "<task>"`; `agents status\|result\|tail\|cancel <id>`; `agents --format json` = capability schema (`cost-routing.md` → Ask flags and lifecycle) |
| Launch or drive codex/gemini/opencode/copilot | `claude-code <v> [args]` (`--` forces passthrough) · `<v> ask\|resume\|review\|transfer\|status\|result\|cancel\|setup\|catalog` · `--print-env` |
| Same from the prompt box | `/coworker:copilot` · `/coworker:opencode` · `/code-worker:gemini` `<verb> \| <task>` (bare task = `ask`); `/code-worker:codex <verb>` (bare = adversarial review); `@agent-<name>` for a subagent |
| Sweep many files in one Gemini call (1M context) | `claude-code ask --agent gemini --tools --dirs <d> [--index] [--diff] [--rubric <file>] "<task>"` (or any agent; large files need `--max-file-bytes`) |
| Review a diff; second opinion on another model | `/code-review` · `claude-code codex review [--adversarial] [--base <b>]` · `@agent-codex` |
| Project agents + CLAUDE.md into every agent CLI | `claude-code agents sync [--agent a] [--dry-run\|--check]` (also run by `install`) |
| Subagent status | `claude-code agents tree [--watch\|--background]` · `agents reap` |

## Agent and command files

- `agents/<name>.md` is the single definition, flat, `name:` equal to the stem. Skeleton:
  `Role · Invocation (agents with a slash entry) · Owns · Knows (when it has on-demand references) · Verbs (when it
  drives a CLI) · Hands off to · Procedure · Report contract · Don't` (`##` sections); `description:`
  says when to use it. `Knows` lists the references the agent reads and when: make knowledge available, not
  mandatory; nothing loads upfront. Memory needs no entry: the dispatch hook appends recall pointers to every brief.
- `commands/<name>.md` is a ~10-line launcher: read `$ARGUMENTS`, run the agent/script, relay the report, point at the
  agent file. Skill shell logic lives in `skills/<name>/scripts/`; anything else in a Go verb or Makefile target.
- Agents hold no memory scope; they PROPOSE memory in their report and the main session writes it.
- `claude-code agents` lists the CLI agents, not this roster.

Dispatch, concurrency, escalation and the test agent: `skills/subagent-workflows/SKILL.md`.

## Authoring: thinking and caching

- Thinking is adaptive; never hand-roll a thinking budget in a skill or command — a model that needs one gets it in
  provider config.
- Mark stable large prefixes with `cache_control`; content that changes per call must stay outside the cached block.
