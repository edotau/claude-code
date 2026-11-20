---
paths:
  - "internal/agent/**"
  - "internal/agentconfig/**"
  - "internal/copilot/**"
  - "internal/setup/install*.go"
---
# Agent CLIs — codex, copilot, opencode, gemini

## Config writes

- Agent CLI configs are merged, never replaced (`~/.codex/config.toml`, `~/.gemini/antigravity-cli/settings.json`,
  Codex/Copilot hooks); keys are referenced by env var and exported only by a routed launch. Each overwrite keeps 5
  timestamped backups; `configure --revert [--agent a]` steps back one.
- opencode's `~/.config/opencode/` gets only the theme file and `agents sync`'s agent files; model/MCP/theme config
  come from a tracked overlay injected at launch (`OPENCODE_CONFIG` / `OPENCODE_TUI_CONFIG`,
  `internal/agentconfig/template/`).
- Codex pins live in the vault codex agent record and apply to `$CODEX_HOME/config.toml`; snapshots and baseline are
  vault-encrypted (`settings_backups`, last 5, excluded from exports). Provider config, hook trust and unknown native
  keys are preserved; an unavailable vault key aborts changed writes.
- Hooks rendered into `~/.codex` carry `HARNESS_HOOK_AGENT=codex`; any `HARNESS_HOOK_*` prefix is harness-owned on
  re-render.

## Dispatch behavior

- `ask --format json` envelope: `agent`, `status` (`ok|error|partial|quota|stalled`), `model`, `answer`, `usage`,
  `duration_ms`, `fallback*`, `handoff`, `transport`; `invoked_model` is the request as asked (`auto`, `<row>:auto` or
  the selector) and `model` the model that ran. Isolation: `--no-persist` (claude, codex), `--safe-mode` (claude,
  codex, opencode), `--debug-file <path>` (claude, gemini).
- `ask --retry <id>` of a run cut off by a deadline, stall, idle or auth-retry runs on claude unless a `--model`,
  `--agent` or `--fallback none` pins the route or claude itself was cut off; it keeps the route and names the move.
  A plain resubmit of the same agent, model and task within 60 minutes of such a cutoff gets the same rule, from
  `state/harness/agent/cutoffs.json` (task hashes only); a successful run on the route clears its entry.
- Without `--tools`, claude and opencode launch no tools, copilot's SDK rejects every request, gemini is only
  instructed not to use them, codex keeps a read-only sandbox shell. `--no-tools` is claude only (others exit 2).
  `--read-only` (implies `--tools`) runs reads, URL fetches and a vetted read-only shell list; other Bash and all
  Write/Edit stay denied, and `--allow` never reopens a deny. codex and gemini refuse `--tools` with
  `--permissions plan`; claude `--tools` off Anthropic gates only explicit rules. Grants and gates:
  `skills/subagent-workflows/references/dispatch-policy-details.md` → Read-only and tools.
- `agents ask --agents <json|file>` defines agents inline (`name`, `description`, `prompt`, `tools`, `model`,
  `max_turns`, `agent`, `profile`, `effort`); `--agent` also takes an `--agents` key. With `--batch`, a line's `agent`
  takes the same selector; `--host` with `--agents` exits 2.
- `agents ask --spec FILE|-|'{...}'`: one JSON object whose keys are ask's own flag names (generated from its FlagSet;
  `prompt` is the task; `overlay`, `agents`, `output-schema` also take an object). An unknown key or bad value exits 2
  naming the nearest key; typed flags beat the spec, typed list flags add to it. `--batch` lines use the same keys
  (`id`, `plan_task` extra; a batch-wide key on a line exits naming it). `--dry-run` prints agent, route (as `gateway
  resolve`), hops, permissions, overlay and the canonical argv through the real resolvers; it saves no record and
  calls no model and stamps neither workspace nor model picks; not with `--live`, `--batch`, `--job`.
- **Overlay** — `agents ask --overlay` / `gateway --overlay` / vault env `COWORKER_OVERLAY`: ONE keyed JSON document
  (`schema` 1) as a file path (regular, yours, not group/world-writable, ≤ 64 KiB), inline `'{...}'` or `-`. Flags and
  prefixes normalise into it; an unknown key or bad value exits 2 naming the valid keys. It rides the run's context,
  never the dispatch's env. Precedence: typed flag > `--overlay`, else `COWORKER_OVERLAY` > `--retry`/`--job` record >
  process env > vault env > agent definition > agent pin > table default (profile and fallback order:
  `providers.md` → Routing precedence). Nested dispatches inherit only `provider`/`profile`; `--host` refuses a
  non-empty env layer; endpoint, proxy, TLS, agent-config and credential env names are refused (use `--profile`).
  Compatibility, relaunch and `--env` rules: `dispatch-policy-details.md` → Overlay.

  `--profile` names a registry row (or `session`); `auto` is only an overlay `profile`, and `ask` and `label` refuse it.
  `ask --provider databricks` without `--profile` resolves to the overlay `auto` pick (least busy workspace serving the
  model); a typed `--profile` or a pin that serves the typed model is unchanged, a pin that lacks it joins the pool.

  | Key | Values | Overrides |
  | --- | --- | --- |
  | `provider` / `profile` | provider key / profile name (`profile` needs `provider`) | `--profile`, provider prefix |
  | `model` / `effort` | model id or alias / effort name | `--model`, `--effort` |
  | `fallback` | `none\|pin\|agents\|all` | `--fallback`, `AGENT_ERROR_FALLBACK` |
  | `host` / `remote` | ssh host (not both, unless `host` names this machine) | `--host`, remote run |
  | `account` | copilot GitHub account (its login) | the credit gate's account pick |
  | `identity` | `auto\|user\|sp` (Databricks) | `--as`, the profile's identity |
  | `codexNetwork` | `true`: codex `--tools` `workspace-write` sandbox gets network (inert under `danger-full-access`; top-level ask only) | sealed network |
  | `cloud` | `true`: diff or repo goes to the vendor's cloud (`claude review`, `codex cloud exec`); own login | local |
  | `maxChildren` | Concurrent children per parent; env > overlay > default; 0 = no cap, invalid = default | 8 |
  | `maxTreeTokens` | Known subtree tokens; env > overlay > default; 0 = no cap, invalid = default | 0 |
  | `budgetUSD` | Claude only: number > 0, passed as `--max-budget-usd` | uncapped |
  | `budgetCredits` | Copilot only: integer >= 30, SDK session credit limit | uncapped |
  | `set` / `unset` / `keep` | env name -> value / names / names | env layer |

- `<agent> models` passes through to the vendor CLI; the catalog is `<agent> catalog`.
- **Model precedence** (`agent.ResolveRun`): a typed model (`--model`, overlay `model`, `<profile>:` prefix), then a
  typed `auto`, then the agent's pinned model (record `model` or `<AGENT>_MODEL`), then the task tier: a
  `Tier: fast|standard|deep` line in the brief wins, else its wording picks (audit, security, parser, design: deep;
  typo, rename, move: fast), and a brief the effort classifier rates high never takes the fast tier.
  A pinned `auto` beats a concrete ladder cell exactly as a typed one does, so the label always equals the
  run. A tier the row
  does not serve falls back to the untiered pick with a note. The ranks are seeded estimates: edit
  `internal/catalog/ranks.json`.
  `--model auto` or `configure agents <agent> model=auto` picks the least-recent eligible model on a rotation row
  (Databricks workspace or copilot GitHub login). Claude's model never rotates: its typed or pinned `auto` runs the
  brief's tier model (haiku fast, sonnet standard or untiered, opus deep); with no route typed or inherited its row
  rotates least-recent over the signed-in workspaces plus its record's `cli` Anthropic row (never the `key` row),
  skipping a held, signed-out or unserving row, and label, `gateway resolve`, `ask --dry-run` and the run all show
  it. Each agent has one history per provider group across dev, qa and prod; holds stay per
  workspace: a pool-pinned pick skips a model where that workspace skips or holds it, and keeps it
  while another workspace serves it. The pool input is the served listing (a GitHub login: its Copilot catalog
  snapshot) alone; there is no built-in seed pool.
  Rotation admits only rows not marked `"rotate": false` in `internal/catalog/ranks.json`; typed, those still run.
  The pool admits only models the agent can run within the tier's `rotation` capability floor and cost cap in
  `internal/catalog/ranks.json` (unranked models skip both where the tier admits them); a tier's
  `defer_cost`/`defer_turns` make a model that dear take one of every `defer_turns` of its turns (standard: cost 5,
  3 turns; a lone or only-remaining dear model still runs). The vault's
  `discovery-models` allowlist only narrows it; held models and, under `--tools`, models probed tools `no` are skipped.
  Outside a rotation row, auto runs the row's tier model (no listing to rank). A cold (missing or stale)
  listing runs the row's tier model with a note; a fresh listing with nothing runnable in the brief's tier and
  nothing held there steps to the standard tier, and only nothing eligible then exits naming held ids, without
  launching; unwritable pick state errors. An untyped, unpinned model keeps the ranked pick.
  Refused auto picks share at most 3 re-picks per ask, including a move after an exhausted pool (a 429 move is
  free); no re-pick follows streamed text or tool execution. Under `--fallback none` an auto-picked workspace
  whose pool is exhausted moves to another workspace of the same provider, never past a typed or inherited
  `--profile`.
  `--model databricks:auto` equals `--provider databricks --model auto`.
  Own-login `<login>/auto` is refused before launch, naming that login's typed ids.
  Labels, `gateway resolve`, and `ask --dry-run` only peek; `agents label <agent> --run` records and freezes the pick
  as `--model` in the `Run:` line. A label for a run that picks itself (`--model auto`, `<row>:auto`, or no model over
  an auto pin) reads `auto (next: <id>)`, since the ask may pick another id; a frozen or typed model keeps the bare id.
  `opencode catalog` text shows `SERVED` for Databricks models: dev/qa/prod marks `y` READY, `n` not READY,
  or `?` for a missing/stale listing.
  `claude-code doctor` auto-pin rows end with 7-day final-route counts when records have `ran_route`.
  A dispatch record includes `attempts` when more than one hop ran (account, profile, model and errors per hop);
  a one-hop run omits it. A foreground ask that moves workspace keeps one record listing every hop.
  `ran_route` names the final launched hop, including an own-login account like `opencode-go`.
  `doctor`'s 7-day rotation summary counts final routes from records with `ran_route`. OpenCode's text catalog
  shows `SERVED` in dev/qa/prod order: `y` READY, `n` not READY, `?` missing/stale cache; login-prefixed ids are blank.
- **opencode** on a Databricks row runs three providers: `<row>` (chat), `<row>-gpt` (Responses), `<row>-claude`
  (Anthropic); an unlisted `--model` is refused at preflight; a model with no openai route is left out of the chat
  list with a note.
- **Claude Code** on a Responses-only model (`gpt-5.5`) is refused at launch and by the router: use codex or copilot.
- **copilot** follows the session profile (then its pin) through the proxy, except a pin on its own GitHub row, which
  decides (D3 exception: the row's `Auth.Login` equals the agent, as gemini on Google) (`/p/<profile>`, BYOK,
  `COPILOT_OFFLINE=true`, no credits); `--model <profile>:<alias>` crosses profiles. The GitHub login is used only
  while the copilot record's GitHub profile (named by its account login, e.g. `aueric_boehcp`; the built-in no-login
  row is `copilot`) has verified credits < 95%, else it falls back to the Databricks profile, never the other GitHub
  account. Usage rows read `databricks/<model>` vs `github·copilot/<model>`; prompt cap = window − 32k − 10%
  (`models.PromptTokens`).
- Interactive copilot on Databricks gets native `/model` via a per-launch registry (0600, per pid) and a login-free
  `COPILOT_HOME` (`state/copilot/registry-home`); an unregistered model fails closed, `--direct` keeps the single-model
  route, macOS skips the registry. `ask --agent copilot` uses the same registry via the SDK.
For codex and copilot, `databricks -p dev` (also `--profile dev` or `--profile=dev`) selects the profile.
Use `claude-code copilot -- databricks` to pass the literal word; everything after the first `--` goes to the
vendor CLI untouched.

- **gemini** runs on `agy`: its Google sign-in by default, or a Databricks profile (`--profile dev|qa|prod`) through
  the agy gateway; `ask --dirs/--files/--diff` inline tracked files only, never `.env*`, `state/` or keys.
- `claude-code doctor --permissions [--agent a] [--format json]` runs a fixed call table through each agent's real
  launch policy per tier (no flag, `--tools`, `--read-only`) with no model call; exit 1 on a missed expectation. Cells
  only a vendor runtime judges are listed apart, uncounted.

## Skills and instructions parity

- **Skills.** `agents sync` links `skills/<name>/SKILL.md` into `~/.agents/skills`, plus copilot's `~/.copilot/skills`
  and agy's `~/.gemini/antigravity-cli/skills` (`internal/agentconfig/project/targets.go`). `skills/synced/**` gets no
  link (opencode reads `~/.claude/skills` natively); sync never prunes a link into it and prints a stale one as `EXTRA`.
- **Portable core.** `CLAUDE.md` (else `AGENTS.md`, `.copilot/copilot-instructions.md`,
  `.github/copilot-instructions.md`: first present) plus unscoped `rules/standards/*.md`, listed by
  `project.PortableCore`; headless and interactive runs load the same core.

| Agent | Reads the core from | Probe |
| --- | --- | --- |
| codex | `~/.codex/AGENTS.md` (rendered by sync) | `project/targets.go` `docs:` |
| gemini (agy) | `~/.gemini/GEMINI.md` (rendered by sync) | same |
| copilot (interactive) | `~/.copilot/copilot-instructions.md` (rendered), not `~/.claude/CLAUDE.md` | `project/targets.go` |
| copilot `ask` (SDK) | appended system text + instruction, skill, agent dirs | `copilot/sdk.go` |
| opencode | `~/.claude/CLAUDE.md` natively; its own `AGENTS.md` **replaces** it, so sync writes none | `project/targets.go` |

- **copilot SDK vs interactive.** Interactive stays `--allow-all`. `ask --agent copilot` runs per-call `Decide` on the
  dispatch's permissions; hooks run natively from `~/.copilot/hooks/*.json`, which the SDK session loads too, so no SDK
  callbacks are registered (each guard would run twice).
- **Fake vendor binaries.** `HARNESS_AGENT_BIN_DIR` makes `paths.AgentBin` read that directory instead of `BinDir()`;
  real vault pins still route.
- **Dispatch depth.** `HARNESS_DISPATCH_MAX_DEPTH` caps nesting (default 3; 0 = no cap; invalid uses default).
  `HARNESS_DISPATCH_MAX_CHILDREN` caps live children per parent (default 8; 0 = no cap; invalid uses default).
  `HARNESS_DISPATCH_MAX_TREE_TOKENS` caps tokens under the root (default 0 = off; unknown usage counts 0; invalid
  uses default). Both resolve env > overlay (`maxChildren` / `maxTreeTokens`) > default, propagate to descendants,
  apply to nested dispatches only, and refuse terminally.
- **Deny and Ask.** Deny is the hard boundary; Ask goes to the agent's own judge where it has one (opencode `--auto`,
  claude on Anthropic). Elsewhere gemini renders Ask as Allow, copilot's SDK approves it, claude off Anthropic denies
  it. A Deny an agent cannot enforce refuses the dispatch (`retry.KindFatal`) naming the rule. `--tools` approves and
  `--workdir` is no write boundary for any agent; scope writes with `--deny`. opencode `--read-only` runs without
  `--auto`; opencode `--safe-mode` refuses any bound tool gate.
- **Bash deny via `permission-gate`.** opencode's native matcher is bypassed by `bash -c "..."`, so the dispatch's
  rules ride `HARNESS_DISPATCH_PERMISSIONS` to the `permission-gate` PreToolUse module (`Decide` per segment): opencode
  runs it from the plugin's `tool.execute.before` (refused under `--safe-mode`, which loads no plugin), codex and agy
  as a native hook (agy also runs `safety-scan` on `run_command`). Modes: `hooks.md` → Applicability. A live codex or
  agy home gets the hooks from `agents sync` or `configure --restore --agents --agent codex|gemini`; until then those
  agents refuse a Bash deny.
- **Memory jobs (claude, gated).** `memory update`/`dream`/catch-up run claude with `RunOptions.Gate`:
  `bypassPermissions` (off Anthropic, `dontAsk` hard-denies writes under `.claude/`), `--strict-mcp-config`, tools
  limited to `Read,Glob,Grep,Edit,Write,MultiEdit,Bash`, and `permission-gate` merged into the launch's one
  `--settings` (never `settings.json`; `exit 2` on any failure). Policy: plan mode, Edit/Write under the job's bank
  dirs, Bash only dream's `claude-code memory review|index|promote` plus `cd` into the store; file redirects are
  refused, `2>&1` is not; Read is denied on `state/vault.db`, `.credentials.json`, `.env` and `*.pem`, which also binds
  read-only Bash: a path spliced in by a substitution is denied, a comment cannot hide a command from the check, and
  `cd` and `echo` are not caught by it; no policy, binary or policy env refuses the launch. A dream whose gate refused
  calls and edited nothing fails and retries in 6 h instead of stamping success. Dream may also run
  `claude-code plan task * --done --sha * --plan <plans>/*` and `… --strike * --plan <plans>/*` (`<plans>` the
  checkout's plans home); `plan task *-reopen*`, `plan tick*` and Edit/Write under `plans/**` (update too) are denied.

## Codex child routing

- Native `spawn_agent` children use the Codex routing cells on a Databricks profile with a validated catalog, only when
  explicitly fresh (`fork_turns="none"` or `fork_context=false`) with no model/effort pin or custom type; the model
  must support Responses and be in the session catalog. A codex record `model` or `effort` disables child routing for
  the session (clear it in `claude-code configure codex`).
- Codex-only hooks log to `state/harness/codex/routing-<session-hash>.jsonl` (0600, no prompts); a decision is verified
  only when the spawn result's agent ID joins a child-start event, else `matches_router_decision` stays `null`.
- `doctor` row `agent flags <agent>` checks the row's `CLI.Flags` against the CLI's help: `ok`,
  `drift: --flag (installed v)` or `skipped: …`; report-only, declared for claude, codex, copilot, gemini and opencode.
- `doctor` inspects Codex machine and `CODEX_HOME` config by metadata only: policy presence leaves the route unverified.
