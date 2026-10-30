# Cost Routing and Cross-Model Dispatches

Claude stays the manager. A step delegated through `claude-code ask` bills to that agent's profile; Claude
spends only the brief and the check. Use another agent when it is cheaper for the step, or when a second model
adds real coverage (independent review, broad-context sweep). Stay inline for small, coupled work.

## Classify, then route

| Kind | Signal | Agent | Call |
|---|---|---|---|
| `sweep` | many files, whole-repo context, web search | `gemini` (agy, Google sign-in) | `claude-code gemini explore "<question>"`; targeted: `claude-code ask --agent gemini --tools "<brief>"`; 1M context: `claude-code gemini bridge --dirs <d> "<task>"` |
| `review` | adversarial second opinion on a diff or plan | `codex` (ChatGPT plan) | `claude-code ask --agent codex --format json "<brief>"`; a diff: `claude-code codex review --adversarial [--base <b>]` |
| `mechanical` | one-liner, lookup, mechanical edit | `copilot` (`haiku`: `internal/routetable/table.json`) | `claude-code ask --agent copilot --tools --workdir <repo> --fallback none --format json "<brief>"` |
| `standard` | everything else (the default) | rotate `claude` · `copilot` · `codex` · `opencode` (`sonnet`: `internal/routetable/table.json`) | as above, `--agent <agent>` |
| `reasoning` | explicitly flagged hard reasoning | Claude (`opus`: `internal/routetable/table.json`) | stays in session; never by default |

Docs and implementation edits rotate across agents including `opencode` (model and effort from
`internal/routetable/table.json`). Architecture, judgement, integration and final verification never leave Claude.
The classifier is `claude-code gateway router --task "<text>"`; a caller's kind is a hint: route by judgement and say
so when you disagree.

Gemini needs `--tools` for any repo or web work — without it agy answers from model memory. Model cell mappings
follow `internal/routetable/table.json` as the single source.

## Run the dispatch

1. `claude-code agents` lists available agents (✓ = binary + credential). Availability is not a working
   credential: smoke-test once with `claude-code ask --agent <a> "Reply READY only."`.
2. Assign agents round-robin across implementation steps and name it in the brief as `Agent: <name>`; keep the
   split even across the session. Assign `Agent: claude` when the brief would cost more than the work, the step
   needs context only this session holds, or the agent failed its smoke test.
3. Size the model alias (`opus`, `sonnet`, `haiku`): `internal/routetable/table.json` is the single source for
   agent × model, effort, account and price. Each dispatch runs the requested alias's cell, including Claude.
4. Use `--fallback none` so a failed dispatch exits non-zero instead of silently running on Claude — see Fallback below.
5. An agent in a shared tree runs in its own clean copy: `skills/subagent-workflows/scripts/isolate.sh <copy>`,
   `--workdir <copy>`, then apply and verify its diff (opencode has ignored `--workdir` and written the live tree).

## Ask flags and lifecycle

Dispatch roles live in `../SKILL.md`; this section is the flag surface every agent shares. Per-agent levels:
`claude-code agents --format json` prints `{name, binary, available, caps{…, cost, background, live, remote}, events[]}`
(`--host <h>` lists a remote's); a flag an agent cannot honor is named in the envelope's `dropped`, never silent.

- **Stream**: `ask --format stream-json` prints NDJSON events (`start|text|tool_use|tool_result|retry|result|error`);
  the `result` event carries the envelope, `usage.cost_usd` where the agent reports it (claude; gemini if agy does).
  Every dispatch also writes `state/harness/dispatch/<id>.events.jsonl`.
- **System prompt and env**: `--system-prompt`/`--append-system-prompt` are native on claude (and codex), emulated as a
  task preamble on the rest (listed in `emulated`). `--env NAME=VALUE` (repeatable) reaches every agent after the
  credential scrub; credential-looking names (`*_API_KEY`, `*_TOKEN`, `*_SECRET`, `*AUTH*`) are refused (use the vault),
  and the dispatch record keeps names only. Not with `--background`.
- **Lifecycle, any agent**: `ask --background` detaches; then `claude-code agents status|result|cancel <id>` and
  `agents tail <id> [-f] [--format json]` (json follows the events file). `agents reap` clears dead dispatches.
- **Remote**: `ask --host <tailnet-host> [--remote-bin <path>]` runs the ask over ssh; profiles resolve on the remote
  vault, `--attach` and `--keep-env` are dropped.
- **Live claude session**: `ask --agent claude --live [--timeout <idle>]` starts a background holder (one claude
  process, unix socket `<id>.sock`); `agents send <id> "<task>"` runs one turn on it and streams its events. One turn
  in flight (a second `send` is refused), 10 minutes idle by default, at most 4 sessions, `agents cancel <id>` ends one.

## Fallback

The router (`internal/router`) dynamically picks and ranks agent + model for every `claude-code ask` dispatch.
Table: `internal/routetable/table.json` defines agent × model alias (`claude`, `copilot`, `codex`, `opencode`,
`gemini` × `opus`, `sonnet`, `haiku`) mapping to model, effort, account key (`<provider>·<profile>`, e.g.
`anthropic·max`, `github·personal`, `openai·chatgpt`) and price. Default weights are `price=0.5` `headroom=0.5`
(vault-overridable via `route` and `routeWeights`). Each dispatch runs the SAME model alias's cell (alias from
`--model`, else the task classifier), including the Claude dispatch; a `<AGENT>_MODEL` pin overrides the
cell (see Pins).

- **Gates** (drop candidate before scoring, with reason):
  - `off` — `<AGENT>_ACTIVE=off` (`claude-code <agent> off`) never routes to that agent, even when asked for it;
    `claude-code <agent> on` lifts it. Set from configure's `Active` row; the vault table shows `ACTIVE` per agent.
    Claude is always on. An explicit `--profile` still runs (D8).
  - `row` — no configured row for the requested model alias.
  - `available` — agent binary or login credentials missing.
  - `account` — Copilot runs on the pinned `COPILOT_PROFILE` only: unpinned, or a pin that is not a gh login,
    drops it (never another profile; `claude-code configure github` changes it).
  - Copilot credit gate — a pinned GitHub profile runs only with credits verified < 95%; at 95% or more (or
    unverified) the dispatch ends as a quota hop and the loop moves to the next agent, carrying any handoff. No fixed
    Databricks profile order: copilot reaches Databricks only through its own pin (`--profile` or `COPILOT_PROFILE`).
    Never a GitHub dispatch over 95%, never a bill.
  - `exhausted` — 100% of window used, or a remembered quota failure (`402`, monthly quota, usage limit).
- **Scoring**: `total = 0.5 * price + 0.5 * headroom` (0–1 each); a subscription's price is its window share left,
  and an account bills the backend the agent actually resolves (codex on an API key = `openai·openai`).
  Low accounts (>=90% of window used) sort after all non-low accounts. Exhausted accounts are never picked.
- **Quota state**: `state/harness/quota.json`, written by `claude-code usage`, by a dispatch's quota failure (resets
  parsed from error message, else window end), and a 15-min targeted refresh. `claude-code doctor` displays a row
  per exhausted or low account.
- **Dispatch ladder** (`ask --fallback all|agents|none`, default `all`): the wanted agent (`--agent`) leads if it
  passes gates; other agent candidates follow sorted by score; Claude runs last under `all`. `agents` stops
  before Claude; `none` fails fast. `review` always falls back across non-Claude agents only, never to Claude.
  `--format json` reports `fallback`/`fallback_reason`; non-empty `fallback` goes on the `Ran on:` line, never hidden.
- **Pins** (`claude-code <agent> profile [<profile>|--clear]` sets or clears `<AGENT>_PROFILE`; `claude-code switch
  <profile>` sets Claude's own pin, `CLAUDE_PROFILE`; configure's `Route` row sets either): a pinned agent's dispatch
  runs only on that profile, never on another account of the same agent. Model precedence: `--model` > `<AGENT>_MODEL`
  pin > the table cell; a pin's model is respelled for its profile. Pins are listed by `claude-code gateway router`.
- **Pinned ladder** (the `all` default; `review` keeps its non-Claude-only rule): a dispatch starts on its agent's pin
  and falls back on quota, rate limit, auth, 5xx, network, generic 400 or context-length overflow. Content-policy
  refusals, HTTP 413 and a bad `--attach` fail fast and stop the loop. The loop always ends at Claude Code's own pin, at
  the lowest model alias whose cell serves the dispatch's model. If Claude's pin is exhausted the error lists every hop
  with its account and says `claude-code switch <profile>`; there is no hop after Claude (`ask --agent claude` with an
  exhausted pin fails fast).
- **Inspection verb**:
  `claude-code gateway router [--model M] [--agent a] [--task "<text>"] [--fallback all|agents|none]`
  `[--format text|json]` prints the pick, per-dimension scores, weights, total, and skip gates; never dispatches.
- **Pressure hooks & workflows**: `agent-effort` hook checks Anthropic quota (>=90% of 5h/weekly window downshifts
  an unset subagent model one alias; >=97% moves code-worker/Explore to `coworker` at haiku with the routed agent
  prepended; opt-out via `HARNESS_ROUTE_SUBAGENTS=0`). The `code-agent` Workflow with no `args.agent` picks via
  `claude-code gateway router --format json`. Finding (2026-10-25): that hook only rewrites an *unset* dispatch model,
  so it never reached the saved Workflows (`workflows/*.js`), which pin every stage explicitly for `agent-effort`;
  each script now reads `claude-code gateway router --agent claude --fallback none` once itself and downshifts its
  own pins under pressure (`saved-workflows.md`).
- **Copilot on Databricks** — dispatches follow the session's `<provider>·<profile>` via the proxy `/p/<profile>`
  (BYOK, `COPILOT_OFFLINE=true`, no credits); `--profile <p> --model <alias>` selects it; prompt cap = window − 32k
  − 10% (≈148k on 200k, ≈868k on a `one_m` profile). Headless mid-task quota writes a handoff in
  `memory/<bank>/handoffs` and returns `KindQuota` with `handoff` in the JSON envelope; the loop carries that
  handoff to the next dispatch (a Databricks dispatch only when a pin or `--profile` names one). Interactive copilot
  cannot hand off. Copilot now switches models within a Databricks profile (`/model`, registry; `--direct` = one model).
- **Explicit profile** (`--profile`): one dispatch, no fallback, but gated and marked on its own account.
- **Credential route** (`internal/vendors/routes.go`, `<agent> setup` → route row): opencode uses its own `opencode`
  login (Go plan; else the vault `OPENCODE_API_KEY`), then its GitHub Copilot login; copilot its pinned GitHub
  profile, then BYOK `ANTHROPIC_API_KEY`. A `<agent>: session profile … cannot serve …; using <x>` line is this
  route, not a fallback dispatch — report `fallback: none`.

Independent read-only dispatches run in parallel (one backgrounded Bash call each); a writer then its reviewer run
in sequence. Dispatches on one provider share its rate limit — reduce concurrency on persistent `429`s.

**Default agent per task class**

| Task class | Agent | Leaf type |
| --- | --- | --- |
| sweep / research | `gemini` | `code-worker`, `Agent: gemini` (or a `gemini` leaf) |
| mechanical edits | `copilot` | `code-worker`, `Agent: copilot` |
| standard edits, docs | rotate `claude` · `copilot` · `codex` · `opencode` | `code-worker`, `Agent: <rotated agent>` |
| review / vet | `codex` | `code-worker`, `Agent: codex` |
| compare, relay, quota offload | several agents | `coworker` (always requests) |
| reasoning / integration | `claude` (in-context) | `claude`, or the main thread |

Leaf types, the `Agent:` field and the label rule: [SKILL.md → Leaf types](../SKILL.md#leaf-types).

## Verify and compare

`--format json` returns `agent`, `provider`, `model`, `usage`, and `fallback`/`fallback_reason` (Fallback above).
Treat the answer as a claim: check `git diff --stat` for the expected files and run the verification yourself.
Each leaf's report carries the contract's `Ran on:` line plus first-try pass/fail and hand fixes (`../SKILL.md` →
Handoff contract); tally per agent (`claude-code ask --log`) and shift the split only on that evidence. A leaf
assigned an agent that did the work itself is a skipped dispatch, not a success.

The agent CLI starts cold: paste every fact it needs; in `~/.claude` add the shared-tree line from SKILL.md.
As a `Workflow` stage (only when the user asks for one): `code-agent {agent, cwd, task}`, or
`code-analyzer {refuters: ["codex"]}`. Interactive agent session: `claude-code codex|gemini|opencode|copilot
[args]` (`--` forces passthrough).
