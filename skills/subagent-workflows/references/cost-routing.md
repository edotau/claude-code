# Cost Routing and Cross-Model Legs

Claude stays the manager. A step delegated through `claude-code ask` bills to that runner's account; Claude
spends only the brief and the check. Use another family when it is cheaper for the step, or when a second model
adds real coverage (independent review, broad-context sweep). Stay inline for small, coupled work.

## Classify, then route

| Kind | Signal | Runner | Call |
|---|---|---|---|
| `sweep` | many files, whole-repo context, web search | `gemini` (agy, Google sign-in) | `claude-code gemini explore "<question>"`; targeted: `claude-code ask --agent gemini --tools "<brief>"`; 1M context: `claude-code gemini bridge --dirs <d> "<task>"` |
| `review` | adversarial second opinion on a diff or plan | `codex` (ChatGPT plan) | `claude-code ask --agent codex --format json "<brief>"`; a diff: `claude-code codex review --adversarial [--base <b>]` |
| `mechanical` | one-liner, lookup, mechanical edit | `copilot` (`haiku` tier: `internal/routetable/table.json`) | `claude-code ask --agent copilot --tools --workdir <repo> --fallback none --format json "<brief>"` |
| `standard` | everything else (the default) | rotate `claude` · `copilot` · `codex` · `opencode` (`sonnet` tier: `internal/routetable/table.json`) | as above, `--agent <runner>` |
| `reasoning` | explicitly flagged hard reasoning | Claude (`opus` tier: `internal/routetable/table.json`) | stays in session; never by default |

Docs and implementation edits rotate across vendors including `opencode` (default `opencode-go/kimi-k3.1-pro` at high
effort). Architecture, judgement, integration and final verification never leave Claude. `--kind` from a caller is
a hint: route by judgement and say so when you disagree.

Gemini needs `--tools` for any repo or web work — without it agy answers from model memory. Tier cell mappings
follow `internal/routetable/table.json` as the single source.

## Run the leg

1. `claude-code agents` lists available runners (✓ = binary + credential). Availability is not a working
   credential: smoke-test once with `claude-code ask --agent <r> "Reply READY only."`.
2. Assign runners round-robin across implementation steps and name it in the brief as `runner: <name>`; keep the
   split even across the session. Assign `runner: claude` when the brief would cost more than the work, the step
   needs context only this session holds, or the runner failed its smoke test.
3. Size the tier (`opus`, `sonnet`, `haiku`): `internal/routetable/table.json` is the single source for runner × tier
   model, effort, account and price. Each leg runs the requested tier's cell, including Claude.
4. Use `--fallback none` so a failed leg exits non-zero instead of silently running on Claude — see Fallback below.
5. A vendor leg in a shared tree runs in its own clean copy: `skills/subagent-workflows/scripts/isolate.sh <copy>`,
   `--workdir <copy>`, then apply and verify its diff (opencode has ignored `--workdir` and written the live tree).

## Fallback

The router (`internal/route`) dynamically picks and ranks runner + model for every `claude-code ask` leg.
Table: `internal/routetable/table.json` defines runner × tier (`claude`, `copilot`, `codex`, `opencode`, `gemini` ×
`opus`, `sonnet`, `haiku`) mapping to model, effort, account_key, and price. Default weights are `price=0.5`
`headroom=0.5` (vault-overridable via `route` and `routeWeights`). Each leg runs the SAME tier's cell (tier
from `--model`, else the task classifier), including the Claude leg.

- **Gates** (drop candidate before scoring, with reason):
  - `row` — no configured row for the requested tier.
  - `available` — runner binary or login credentials missing.
  - `account` — Copilot runs on the pinned `COPILOT_GITHUB_USER` only: unpinned, or a pin that is not a gh login,
    drops it (never another account; `claude-code configure github` changes it).
  - Copilot credit gate — the GitHub login runs only with credits verified < 95%; otherwise (or unverified) the leg
    uses the vault's Databricks profile (`HARNESS_PROVIDER` pin if Databricks, else first OK dev/qa/prod) and prints
    `copilot: <user> N% of credits used — using databricks profile <p>`. Never a GitHub leg, never a bill.
  - `exhausted` — 100% of window used, or a remembered quota failure (`402`, monthly quota, usage limit).
- **Scoring**: `total = 0.5 * price + 0.5 * headroom` (0–1 each); a subscription's price is its window share left,
  and an account bills the backend the runner actually resolves (codex on an API key = `codex:openai`).
  Low accounts (>=90% of window used) sort after all non-low accounts. Exhausted accounts are never picked.
- **Quota state**: `state/harness/quota.json`, written by `claude-code usage`, by a leg's quota failure (resets
  parsed from error message, else window end), and a 15-min targeted refresh. `claude-code doctor` displays a row
  per exhausted or low account.
- **Dispatch ladder** (`ask --fallback all|vendors|none`, default `all`): the wanted runner (`--agent`) leads if it
  passes gates; other vendor candidates follow sorted by score; Claude runs last under `all`. `vendors` stops
  before Claude; `none` fails fast. `review` always falls back across vendors only, never to Claude. `--format json`
  reports `fallback`/`fallback_reason`; non-empty `fallback` goes on the `Ran on:` line, never hidden.
- **Inspection verb**:
  `claude-code gateway route [--tier T] [--agent a] [--task "<text>"] [--fallback all|vendors|none]`
  `[--format text|json]` prints the pick, per-dimension scores, weights, total, and skip gates; never dispatches.
- **Pressure hooks & workflows**: `agent-effort` hook checks Anthropic quota (>=90% of 5h/weekly window downshifts
  an unset subagent model one tier; >=97% moves code-worker/Explore to `coworker` at haiku with routed runner
  prepended; opt-out via `HARNESS_ROUTE_SUBAGENTS=0`). The `code-agent` Workflow with no `args.leg` picks via
  `claude-code gateway route --format json`. Finding (2026-10-25): that hook only rewrites an *unset* dispatch model, so
  it
  never reached the saved Workflows (`workflows/*.js`), which pin every stage explicitly for `workflow-lint`; each
  script now reads `claude-code gateway route --agent claude --fallback none` once itself and downshifts its own pins
  under
  pressure (`saved-workflows.md`).
- **Copilot on Databricks** — legs follow the session's `<provider>·<profile>` via router `/p/<profile>` (BYOK,
  `COPILOT_OFFLINE=true`, no credits); `--model <profile>:<tier>` works; prompt cap = window − 32k − 10% (≈148k on 200k,
  ≈868k on a `one_m` profile). Headless mid-task quota writes a handoff in `memory/<bank>/handoffs` and resumes once on
  Databricks, else returns `KindQuota` with `handoff` in the JSON envelope; interactive copilot cannot hand off.
- **Explicit backend** (`--provider`, `provider:model`): one leg, no fallback, but gated and marked on its own account.
- **Credential route** (`internal/vendors/routes.go`, `<vendor> setup` → route row): opencode uses its own `opencode`
  login (Go plan; else the vault `OPENCODE_API_KEY`), then its GitHub Copilot login; copilot its pinned GitHub login,
  then BYOK `ANTHROPIC_API_KEY`. A `<vendor>: session provider … cannot serve …; using <x>` line is this route, not
  a fallback leg — report `fallback: none`.

Independent read-only legs run in parallel (one backgrounded Bash call each); a writer then its reviewer run in
sequence. Legs on one provider share its rate limit — reduce concurrency on persistent `429`s.

**Default runner per task class**

| Task class | Runner |
| --- | --- |
| sweep / research | `gemini` |
| mechanical edits | `copilot` |
| standard edits, docs | `opencode` |
| review / vet | `codex` |
| reasoning / integration | `claude` (in-context) |

## Verify and compare

`--format json` returns `agent`, `provider`, `model`, `usage`, and `fallback`/`fallback_reason` (Fallback above).
Treat the answer as a claim: check `git diff --stat` for the expected files and run the verification yourself.
Each leaf's report carries the contract's `Ran on:` line plus first-try pass/fail and hand fixes (`../SKILL.md` →
Handoff contract); tally per runner (`claude-code ask --log`) and shift the split only on that evidence. A leaf
assigned a runner that did the work itself is a skipped leg, not a success.

The vendor CLI starts cold: paste every fact it needs; in `~/.claude` add the shared-tree line from SKILL.md.
As a `Workflow` stage (only when the user asks for one): `code-agent {leg, cwd, task}`, or
`code-analyzer {refuters: ["codex"]}`. Interactive vendor session: `claude-code codex|gemini|opencode|copilot
[args]` (`--` forces passthrough).
