# Cost Routing and Cross-Model Dispatches

Claude stays the manager: a step sent through `claude-code ask` or `claude-code dispatch` bills the agent's profile,
and Claude spends only the brief and the check. Delegate when another agent is cheaper for the step or adds real
coverage; stay inline for small, coupled work. Architecture, judgement, integration and final verification never leave
Claude.

## Pick by role

| Role | Agent | Call |
| --- | --- | --- |
| explore — code scan + web/deep research | `gemini` | `claude-code dispatch explore [--part q]… "<task>"` (≤12 parallel legs, one cited report) |
| wide edits, scans, audits | `copilot` fleet | `claude-code dispatch fleet "<task>"` or `ask --agent copilot --fleet`; check `git diff --stat` after |
| compare or find a model | `opencode` | `claude-code dispatch compare --models a,b "<task>"`; pool: `opencode catalog list`; `--model byok` = router picks a rare Databricks model |
| adversarial review | `codex` | `claude-code codex review --adversarial [--base <b>]` |
| standard / mechanical edits | router pick | `claude-code ask --agent <a> --tools --fallback none --format json "<brief>"` |
| hard reasoning, integration | Claude | stays in session |

Gemini needs `--tools` for repo or web work (without it agy answers from memory); `dispatch explore` sets it.

## Run the dispatch

1. `claude-code agents` lists agents (✓ = binary + credential); availability isn't a working login, so smoke once:
   `claude-code ask --agent <a> "Reply READY only."`.
2. Name the agent in the brief (`Agent: <name>`); use `Agent: claude` when the brief costs more than the work, the step
   needs this session's context, or the agent failed its smoke test.
3. Name the model alias (`opus`/`sonnet`/`haiku`); `internal/router/table.json` maps agent × alias → model, effort,
   account and price.
4. `--fallback none` makes a failed dispatch exit non-zero instead of silently running on Claude.
5. The agent CLI starts cold: paste every fact it needs, plus the shared-tree line from SKILL.md. opencode has ignored
   `--workdir` and written the live tree — check `git diff --stat`.

## Concurrency

Under a batch (`ask --batch`, `dispatch`, workflows) each billed account runs up to 12 dispatches at once
(`inflight.Base`; a profile's `concurrency` overrides) and the machine up to min(12 × accounts, 4 × CPUs); outside a
batch nothing caps them. Legs run host-isolated (no other profile's credentials). Back off on persistent `429`s.

## Ask flags and lifecycle

A flag an agent can't honor is named in the envelope's `dropped`, never silent; `claude-code agents --format json`
lists each agent's caps.

- **Stream**: `ask --format stream-json` (NDJSON `start|text|tool_use|tool_result|retry|result|error`; `result` holds
  the envelope). Every dispatch also writes `state/harness/dispatch/<id>.events.jsonl`. Go: `agent.Client.Stream`.
- **Prompt and env**: `--system-prompt`/`--append-system-prompt` are native on claude and codex, emulated elsewhere;
  `--env NAME=VALUE` reaches every agent, credential-looking names refused (use the vault).
- **Lifecycle**: `ask --background`, then `claude-code agents status|result|tail|cancel <id>`; `agents reap` cleans up.
- **Remote / live**: `ask --host <h>` runs over ssh; `ask --agent claude --live` + `agents send <id> "<task>"` holds one
  claude process (one turn in flight, ≤4 sessions).

## Fallback

The router ranks candidates for every dispatch; `claude-code gateway router [--agent a] [--model M] [--task "<t>"]`
shows the pick, scores and skip gates without dispatching.

- **Ladder**: candidates follow `routing.ladders` (`internal/providers/defaults.json`). Default: copilot (gpt-6.1-sol,
  gpt-6-astra) → Databricks → codex (openai) → opencode, Claude scored in after copilot; `--route first-party`:
  copilot → codex → gemini → opencode-go. The `--agent` asked for leads when it passes the gates.
- **Gates**: `off` (`claude-code <agent> off`), no table row, binary/login missing, Copilot's pinned GitHub profile
  only while credits are verified < 95%, an exhausted account (100% window or a remembered `402`/quota error).
- **Score**: `0.5 × price + 0.5 × headroom`; accounts ≥90% used sort last.
- **`--fallback all|agents|none`** (default `all`): `agents` stops before Claude, `none` fails fast; review never falls
  back to Claude. A non-empty `fallback` goes on the `Ran on:` line.
- **Pins**: `claude-code <agent> profile <p>` keeps that agent on one profile; `--model` > `<AGENT>_MODEL` > table cell.
  `--profile` is one dispatch with no fallback.
- A `<agent>: session profile … cannot serve …; using <x>` line is the credential route, not a fallback: report
  `fallback: none`. Copilot-on-Databricks details: `rules/harness/agent.md` → Dispatch behavior.

## Verify and compare

Treat every answer as a claim: check `git diff --stat` for the expected files and run the verification yourself. A
leaf told to use an agent that did the work itself is a skipped dispatch, not a success. Tally per agent
(`claude-code ask --log`) and shift the split only on that evidence.
