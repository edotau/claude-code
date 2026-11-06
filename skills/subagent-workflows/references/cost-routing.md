# Cost Routing and Cross-Model Dispatches

Serves `../SKILL.md` Phase 0 (shape) and Phase 2 (dispatch). `ask`/`dispatch` bill the agent's profile: delegate when
cheaper or wider; integration and final checks stay Claude's.

## Pick by role

| Role | Agent | Call |
| --- | --- | --- |
| explore — code scan + web/deep research | `gemini` | `claude-code agents dispatch explore [--part q]… "<task>"` (≤12 parallel legs, one cited report) |
| wide edits, scans, audits | `copilot` fleet | `claude-code agents dispatch fleet "<task>"` or `ask --agent copilot --fleet`; check `git diff --stat` after |
| compare or find a model | `opencode` | `claude-code agents dispatch compare --models a,b "<task>"`; pool: `opencode catalog list`; `--model discovery` = router picks a rare Databricks model |
| diff review | `codex` · `gemini` | `claude-code codex review --adversarial [--base <b>]` · `claude-code gemini review --model gemini-4-argon-high` |
| standard / mechanical edits | router pick | `claude-code agents ask --agent <a> --tools --fallback none --format json "<brief>"` |
| hard reasoning, integration | Claude | stays in session |

Gemini needs `--tools` for repo or web work (else agy answers from memory); `dispatch explore` sets it. Copilot off
Databricks uses `--model gpt-6.1-sol`; `--model discovery` needs a Databricks `Profile:`.

## Run the dispatch

Smoke each agent once: `claude-code agents ask --agent <a> "Reply READY only."` (`agents` ✓ = binary + credential). Name
it in the brief (`Agent: <name>`), or `Agent: claude` when the brief costs more than the work, the step needs this
session's context, or the smoke failed. Model alias (`opus`/`sonnet`/`haiku`): `internal/router/table.json` maps agent ×
alias → model, effort, account and price. The CLI starts cold: paste every fact plus the shared-tree line; opencode has
ignored `--workdir` and written the live tree, so check `git diff --stat`. In a batch
(`agents ask --batch`, `agents dispatch`, workflows) each account runs up to its cap (`inflight.Base`; a profile's `concurrency` overrides), the machine up to
min(cap × accounts, 4 × CPUs); outside one nothing caps. Back off on `429`s.

## Ask flags and lifecycle

A flag an agent can't honor is named in the envelope's `dropped`, never silent. `ask --format stream-json` emits NDJSON
events (`result` holds the envelope; each dispatch also writes `state/harness/dispatch/<id>.events.jsonl`); `--env
NAME=VALUE` reaches every agent (credential-looking names refused); `--host <h>` runs over ssh; `--agent claude|gemini
--live` holds one session open and `agents send <id> "<task>"` feeds it (one turn in flight, ≤4 sessions).

## Fallback

- **Ladder**: `routing.ladders` (`internal/providers/defaults.json`); `claude-code gateway router [--agent a] [--model
  M] [--task "<t>"]` shows the pick, scores and skip gates. Default: copilot (gpt-6.1-sol, gpt-6-astra) → Databricks →
  codex (openai) → opencode, Claude scored in after copilot; `--route first-party`: copilot → codex → gemini →
  opencode-go. The `--agent` asked for leads when it passes the gates.
- **Gates**: `off` (`claude-code <agent> off`), no table row, binary/login missing, Copilot's pinned GitHub profile only
  while credits are verified < 95%, an exhausted account (100% window or a remembered `402`/quota error). **Score**:
  `0.5 × price + 0.5 × headroom`; accounts ≥90% used sort last.
- **`--fallback all|agents|none`** (default `all`): `agents` stops before Claude, `none` fails fast instead of running
  on Claude; review never falls back to Claude. A non-empty `fallback` goes on the `Ran on:` line.
- **Pins**: `claude-code <agent> profile <p>` keeps that agent on one profile; `--model` > `<AGENT>_MODEL` > table cell;
  `--profile` is one dispatch with no fallback.
- A `<agent>: session profile … cannot serve …; using <x>` line is the credential route, not a fallback: report
  `fallback: none`. Copilot-on-Databricks: `rules/harness/agent.md` → Dispatch behavior.

## Verify and compare

Treat every answer as a claim: check `git diff --stat`, run the verification yourself. A leaf told to use an agent
that did the work itself skipped the dispatch. Tally per agent: `agents ask --log`.
