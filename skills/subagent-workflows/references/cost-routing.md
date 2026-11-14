# Cost Routing and Cross-Model Dispatches

Serves `../SKILL.md` Phase 0 (shape) and Phase 2 (dispatch). `ask`/`dispatch` bill the agent's profile: delegate when
cheaper or wider; integration and final checks stay Claude's.

## Dispatch and routing

Agent roles and the relay contract: `../SKILL.md` → Leaf types. Use the root machine's pins; pass an explicit route
or model only when the task calls for one. Do not pin `dev`, `qa`, `prod` or a personal login based on historical
measurements. Aliases come from `internal/providers/defaults.json` and the vault overlay, resolved by
`internal/models`; `internal/router/table.json` holds price/account rows and the Databricks tier buckets (the
models an alias picks on a workspace; `rules/harness/providers.md` → Model selection).

### Shell roles

For an explicitly requested script or shell task, `claude-code agents dispatch` supplies these built-in roles;
the vault's dispatch-roles overlay may replace them:

| Role | Execution |
| --- | --- |
| `explore` | copilot, one read-only code/web question per `--part`, up to 12 |
| `fleet` | copilot fleet mode; inspect every changed file |
| `compare` | opencode, one call per model in `--models a,b` |

These are shell conveniences, not substitutes for visible Agent leaves. A relay runs one task; it does not invoke
a role fan-out unless explicitly requested. `gemini` needs tools for file reads or search; use its read-only mode
for research. An OpenCode run may write outside its intended workdir: verify the actual tree and owned files.

## Ask flags and lifecycle

A flag an agent can't honor is named in the envelope's `dropped`, never silent. `ask --format stream-json` emits NDJSON
events (`result` holds the envelope; each dispatch also writes `state/harness/dispatch/<id>.events.jsonl`); `--env
NAME=VALUE` reaches every agent (credential-looking names refused); `--host <h>` runs the ask on that host over ssh
(`rules/harness/network.md`); `--agent claude|gemini --live` holds one session open and `agents send <id> "<task>"`
feeds it (one turn in flight, ≤4 sessions).

## Fallback

- **Order**: the requested target (agent, `--profile` or overlay `profile` workspace, `--model`), then the agent's own
  pin (its vault profile and model; skipped when it has none or the pin is what was requested), then Claude on the
  current session. Nothing else: no sibling workspace, no login but the pin's, no other agent.
  `claude-code gateway --agent <a>` shows the resolved order and skip gates. The overlay `provider` chooses an
  execution arm; `--profile` leads on a profile and `--fallback none` pins that call. Neither changes persistent pins.
- **Pin hop left out**: inside an overlay `provider: databricks, profile: <p>` when the pin is another workspace
  (the arm refuses it; the skip reads `outside databricks:<p>`), and with `--native`. An own-login pin (copilot's
  GitHub profile) stays, so unless `--fallback none` the confined dispatch can end on that login, then `claude`.
- **Gates**: `off` (`claude-code <agent> profile --off`), no table row, binary/login missing, Copilot's pinned GitHub
  profile only while credits are verified < 95%, an exhausted account (100% window or a remembered `402`/quota
  error). A gated step is skipped; the order does not change.
- **`--fallback all|agents|none`** (default `all` when `AGENT_ERROR_FALLBACK` is set in the shell or vault env, else
  `none`): `all` is the full order, `agents` stops after the pin, `none` runs the requested target only and exits
  non-zero with the reason; review never falls back to Claude. A non-empty `fallback` goes on the `Ran on:` line.
- **Announced**: a target the preflight refuses prints `preflight: <requested> failed <check> (<detail>); chose
  <chosen>` (`preflight: note: <text>` for a skipped check) and lands in the dispatch record's `preflight` object.
  A run that ended on another step reads `ask: ran on <agent> · <account> · <model> · fallback from <requested>
  (<first reason>)`; the envelope carries `requested`. Codex asked for a model its workspace cannot serve over
  Responses fails the `wire` check instead of running another model.
- **Pins**: `claude-code <agent> profile <p>` keeps that agent on one profile; `--model` > `<AGENT>_MODEL` > table cell;
  `--profile` is one dispatch that pins with `--fallback none` and leads (still falling back) with any other
  explicit `--fallback`.
- A `<agent>: session profile … cannot serve …; using <x>` line is the credential route, not a fallback: report
  `fallback: none`. Copilot-on-Databricks: `rules/harness/agent.md` → Dispatch behavior.

## Verify and compare

Fan-in: `../SKILL.md` → Fan-in. A leaf told to use an agent that did the work itself skipped the dispatch. Tally per
agent: `agents status`.
