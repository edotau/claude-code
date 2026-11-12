# Cost Routing and Cross-Model Dispatches

Serves `../SKILL.md` Phase 0 (shape) and Phase 2 (dispatch). `ask`/`dispatch` bill the agent's profile: delegate when
cheaper or wider; integration and final checks stay Claude's.

## Dispatch and routing

Agent roles and the relay contract: `../SKILL.md` → Leaf types. Manager dispatch: `CLAUDE.md` guideline 3.
Use the root machine's pins; pass an explicit route or model only when the task calls for one. Do not pin `dev`,
`qa`, `prod` or a personal login based on historical measurements. `claude-code gateway --agent <a>` previews the
current pick; `claude-code <agent> catalog` lists models. Aliases come from `internal/providers/defaults.json`
and the vault overlay, resolved by `internal/models`; `internal/router/table.json` holds price/account rows.

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
NAME=VALUE` reaches every agent (credential-looking names refused); `--host <h>` runs over ssh; `--agent claude|gemini
--live` holds one session open and `agents send <id> "<task>"` feeds it (one turn in flight, ≤4 sessions).

## Fallback

- **Ladder**: `routing.ladders` in `internal/providers/defaults.json` plus vault overrides.
  `claude-code gateway --agent <a>` shows the resolved pick and skip gates. `--route` chooses an execution arm;
  `--profile` leads on a profile and `--fallback none` pins that call. Neither changes persistent pins.
- **Gates**: `off` (`claude-code <agent> off`), no table row, binary/login missing, Copilot's pinned GitHub profile only
  while credits are verified < 95%, an exhausted account (100% window or a remembered `402`/quota error). **Score**:
  `0.5 × price + 0.5 × headroom`; accounts ≥90% used sort last.
- **`--fallback all|agents|none`** (default `all` when `AGENT_ERROR_FALLBACK` is set in the shell or vault env, else
  `none`): `agents` stops before Claude, `none` fails fast instead of running
  on Claude; review never falls back to Claude. A non-empty `fallback` goes on the `Ran on:` line.
- **Pins**: `claude-code <agent> profile <p>` keeps that agent on one profile; `--model` > `<AGENT>_MODEL` > table cell;
  `--profile` is one dispatch that pins with `--fallback none` and leads (still falling back) with any other
  explicit `--fallback`.
- A `<agent>: session profile … cannot serve …; using <x>` line is the credential route, not a fallback: report
  `fallback: none`. Copilot-on-Databricks: `rules/harness/agent.md` → Dispatch behavior.

## Verify and compare

Treat every answer as a claim: check `git diff --stat`, run the verification yourself. A leaf told to use an agent
that did the work itself skipped the dispatch. Tally per agent: `agents ask --log`.
