---
paths:
  - "internal/agent/**"
  - "internal/agentconfig/**"
  - "internal/copilot/**"
  - "internal/setup/install*.go"
---
# Agent CLIs — codex, copilot, opencode, gemini, pi

## Config writes

- Agent CLI configs are merged, never replaced (`~/.codex/config.toml`, `~/.gemini/antigravity-cli/settings.json`,
  Codex/Copilot hooks); keys are referenced by env var and exported only by a routed launch. Each overwrite keeps 5
  timestamped backups; `configure --revert [--agent a]` steps back one.
- pi runs from a harness-generated private agent dir (`PI_CODING_AGENT_DIR` under the harness state dir); nothing is
  written to `~/.pi`.
- opencode's `~/.config/opencode/` gets only the theme file and `agents sync`'s agent files; model/MCP/theme config
  come from a tracked overlay injected at launch (`OPENCODE_CONFIG` / `OPENCODE_TUI_CONFIG`,
  `internal/agentconfig/template/`).
- Codex pins live in the vault codex agent record and apply to `$CODEX_HOME/config.toml`; snapshots and baseline are
  vault-encrypted (`settings_backups`, last 5, excluded from exports). Provider config, hook trust and unknown native
  keys are preserved; an unavailable vault key aborts changed writes.
- The file's route follows the session profile unless the codex record's `config_route` (`session` or a route object;
  `configure codex` or `configure agents codex config_route=<r>`) pins it. A changed provider, model
  or catalog prints a restart notice (ChatGPT app, Codex app-server); the file's helper `timeout_ms` is 100000 (`-c`
  stays 15000).
- Hooks rendered into `~/.codex` carry `HARNESS_HOOK_AGENT=codex`; any `HARNESS_HOOK_*` prefix is harness-owned on
  re-render.

## Dispatch behavior

- `ask --format json` envelope: `agent`, `status` (`ok|error|partial|quota|stalled`), `model`, `answer`, `usage`,
  `duration_ms`, `fallback*`, `handoff`, `transport`, `route`/`requested_route` objects (`provider` kept one release);
  `invoked_model` is the plain model asked (`auto` or the selector; the profile is the route's) and `model` the model
  that ran. Isolation: `--no-persist` (claude, codex), `--safe-mode` (claude, codex, opencode), `--debug-file <path>`
  (claude, gemini).
- `ask --retry <id>` of a run cut off by a deadline, stall, idle or auth-retry runs on claude unless a `--model`,
  `--agent` or `--fallback none` pins the route or claude itself was cut off; it keeps the route and names the move.
  A plain resubmit of the same agent, model and task within 60 minutes of such a cutoff gets the same rule, from
  `state/harness/agent/cutoffs.json` (task hashes only); a successful run on the route clears its entry.
- `agents ask --loop "<goal>"` (top level only) plans rounds of read-only parts on `--loop-planner` (claude), runs
  them on the `--batch` pool, retries a failed part once on claude (not a typed model or `--fallback none`) and keeps
  state in `state/harness/ask/loops/<id>.json`: exit 0 done, 1 `needs_main`, 4 cap or budget, `--loop-resume <id>`.
- Without `--tools`, claude and opencode launch no tools, copilot's SDK rejects every request, gemini is only
  instructed not to use them, codex keeps a read-only sandbox shell. `--no-tools` is claude only (others exit 2).
  `--read-only` (implies `--tools`) runs reads, URL fetches and a vetted read-only shell list; other Bash and all
  Write/Edit stay denied, and `--allow` never reopens a deny. codex and gemini refuse `--tools` with
  `--permissions plan`; claude `--tools` off Anthropic gates only explicit rules. Grants and gates:
  `skills/subagent-workflows/references/dispatch-policy-details.md` → Read-only and tools.
- **pi** (npm `@earendil-works/pi-coding-agent`, MIT, pinned 1.6.0, Node 22.19+) runs by `agents ask --agent pi` and
  by `claude-code pi` (below), on a Databricks profile through the loopback proxy, with no own login. It runs the
  models on the profile's Pi menu: an id not on it is refused ("not on the menu") and never enters the auto pool. With
  no tools flag it runs with no tools. Any `--tools` run runs Pi with `--no-builtin-tools` plus the harness's guard
  extension (the first `-e`, run dir, 0600), whose wrapped tools are the only ones Pi has: each call is judged by
  `hook PreToolUse --pi-guard` (path normalized as Pi does and judged as typed and as the disk spells it,
  `safety-scan` and `permission-gate`) and fails closed (a guard that fails to load leaves Pi no tools; it refuses any
  Pi but the pinned one); a cancelled or timed-out run gets SIGTERM, a 2 s grace, then every descendant is killed
  (Pi's bash groups included), and the idle watch is held while any tool call runs; Pi proves it loaded by writing the
  run's nonce, checked on the first stream event, else the run is killed and refused ("pi guard did not load; run
  refused"). Bare `--tools` gives read, bash, edit, write, grep, find and ls (never `powershell`: unmapped, blocked);
  `--read-only` and `--permissions plan` give read, grep, find and ls only (no shell, stronger than other agents'
  vetted list); a Deny on Edit or Write drops bash (the gate cannot tie a shell command to a file), on Read also grep,
  on Glob or Grep also grep, find and ls. `--allow`, `--deny` and `--permissions` ride `HARNESS_DISPATCH_PERMISSIONS`
  to the gate as for opencode; an `--ask` rule is refused (the guard only allows or blocks), as is an `--env` naming
  `PI_CODING_AGENT_DIR`, `HARNESS_PI_*`, `HARNESS_HOOK_*` or `HARNESS_DISPATCH_PERMISSIONS`. Pi has no permission
  prompts or sandbox of its own. A dispatch passes `--no-context-files` (no CLAUDE.md or AGENTS.md), `--no-mcp` and,
  so a repo's own `.pi` never runs code with your privileges, `--no-approve --no-extensions --no-skills
  --no-prompt-templates --no-themes`; `--effort` maps to `--thinking` (minimal to max); `--model auto` and tier
  aliases follow the other agents' rules. Wire routing is per model (`internal/pi/config.go` `pickWire`): Claude ids
  on Anthropic Messages, any non-Claude, non-Grok model that serves Responses on the Responses wire, everything else
  and Grok on chat completions; a model whose wire has no route on the profile is dropped from the menu. Pi exits 0
  even when a request fails, so the harness judges the run from its JSON event stream: success needs `agent_settled`,
  a last message stopped `stop` and no open tool call (`length`, `aborted`, `toolUse` left unsettled, unknown stop
  reasons and cut-off streams fail). Pi is no longer read-only, so it may be a fallback for write work. A pi leaf
  launched through the `pi` subagent relay carries a file deny on the live checkout, so it has NO shell: give it
  edit-only tasks and run the build, tests and live check in the main session or a paired gate leaf. The tool-name
  mapping and the guard's modules: `/Users/edotau/.claude/rules/harness/hooks.md` → Pi · plugin.
- **pi interactive** — `claude-code pi [--profile dev|qa|prod] [--read-only] [--print-env]` opens Pi's TUI behind the
  same guard (first `-e`, `--no-builtin-tools`): all seven tools, or read, grep, find and ls with `--read-only`. With
  no pin and no `--profile` it picks a profile from the Databricks pool and says which. Pi has no login of its own, so
  it never launches plain and `--direct` is refused. Context files (`AGENTS.md`, `CLAUDE.md`) load here, not in a
  dispatch; the work dir's `.pi` extensions, packages and settings never load. Sessions are kept in
  `state/harness/pi/sessions`. `--print-env` is a preview: it leaves no run dir. Pi args (after `--`, or the record's
  `launch.args`) pass only from an allow-list: `-c`/`--continue`, `-r`/`--resume`, `--session <id>`,
  `-n`/`--name <name>`, `--thinking <level>`, `-p`/`--print`, `--verbose`, `--offline` and a prompt; any other flag
  (`-e`, `-a`/`--approve`, `--skill`, `--tools`, `--model`, `--mode`, ...) or `@file` is refused with the harness
  way to do it. A dispatch passes its prompt after `--` (a prompt like `-a` stays text; a leading `@` gets a space).
- **pi packages** — the owner pins an exact list in the Pi record:
  `claude-code configure agents pi packages=<spec>[,<spec>...]`. A spec is `npm:[@scope/]name@<exact x.y.z>`,
  `git:<host>/<owner>/<repo>@<40-hex commit>` (a branch or tag is refused: only a commit is immutable) or a local
  path (absolute or `./relative`, resolved against the config dir). Packages install into a private store in
  `state/harness/pi/packages` (its real path must sit inside the state dir, else nothing installs or is removed),
  credential-free. npm runs with `--ignore-scripts --userconfig=<null> --globalconfig=<null>` and no inherited
  `npm_config_*`; git runs with system and global config off, hooks, fsmonitor, symlinks, submodules and the file and
  ext transports off, fetching the pinned commit only (shallow, else the branch heads; no tags), and the checked-out
  HEAD must equal the pin. A launch reviews each package again with Pi's own manifest rules (exact `pi` key, each
  resource an array of strings, else refused) and refuses a no-code package with a symlink leaving it. A package that
  ships code (extensions, a bare package dir Pi would load as an extension, a `bin`, or lifecycle scripts) is refused
  (a fresh install is removed) unless its exact spec is in `packages_approved`
  (`claude-code configure agents pi packages_approved=<spec>`); a git package's dependencies install only after that
  approval, and never when the repo ships an `.npmrc`. Approval is per exact spec: a new version needs a new approval.
  Only the owner's own session sets `packages` or `packages_approved`: `configure` refuses both inside a dispatch
  (`HARNESS_DISPATCH_*` set; advisory: a leaf that unsets them, or an in-session subagent, is not caught). A launch
  never removes a slot: `claude-code clean` (target `pi-packages`) removes slots the record no longer lists, only while
  no pi run is live. Nothing is updated automatically. Packages load after the guard, so a package cannot replace a
  guarded tool; a package's own tools are blocked (its skills, prompts, themes and commands work).
- **pi package risk** — **a Pi package's extension code runs inside the Pi process with the user's privileges. It is
  not sandboxed and the guard does not judge it.** It can run code on load, patch the guard, read the environment and
  make network calls. The automatic review covers only manifest keys, whether code ships and the exact version. The
  owner's approval is the only control over what that code does. The content of a local-path package can change
  after approval and is not hashed: each launch re-reviews it (new code needs approval), but approved code that
  changes, or changes during a run (`/reload`), loads unchecked.
- `claude-code doctor --permissions` lists pi, the wave owned-files gate covers pi, and `doctor` has the rows
  `pi route`, `tool pi` and `pi packages`.
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
  (`schema` 1) as a file path (regular, yours, not group/world-writable, ≤ 64 KiB), inline `'{...}'` or `-`. Flags
  normalise into it; an unknown key or bad value exits 2 naming the valid keys. It rides the run's context,
  never the dispatch's env. A route is ONE unit (provider, profile, identity, account, host, remote) taken whole from
  the highest source that sets any of it: command line (flags + `--overlay`) > `COWORKER_OVERLAY` > `--retry`/`--job`
  record > agent definition > vault record > session > registry default; settings (model, effort, fallback…) merge per
  key (order, fallback: `providers.md` → Routing precedence). An inherited `COWORKER_OVERLAY` from a parent dispatch
  is a fence, not a source; `--host` refuses a non-empty env layer; endpoint, proxy, TLS, agent-config and credential
  env names are refused (use `--profile`). Compatibility, relaunch and `--env` rules: `dispatch-policy-details.md`.

  An object that sets any route key names provider AND profile (a github account route: provider `github` + `account`);
  a partial object exits 2 naming the missing key. Shorthand: `--profile R` fills provider from R's own
  registry row, never the session; `--provider P` = `{P, auto}`; `databricks` = the pool pick (least busy workspace
  serving the model); `auto` is only an overlay `profile`: `ask --profile auto` and `label` refuse it. A flag and
  `--overlay` setting one key differently on one command line exit 2 naming both; `--spec` stays a template typed
  flags beat. Interactive launchers accept route, `model` and `effort` in `--overlay`; `fallback`, budgets, caps,
  `codexNetwork` and `cloud` are refused naming the key. `Run:` lines and held-job hints print `--overlay
  '<object>'`; batch lines take an `overlay` key.

  | Key | Values | Overrides |
  | --- | --- | --- |
  | `provider` / `profile` | provider key / profile name (both, or neither) | `--provider`, `--profile` |
  | `model` / `effort` | model id or alias / effort name | `--model`, `--effort` |
  | `fallback` | `none\|pin\|agents\|all` | `--fallback`, `AGENT_ERROR_FALLBACK` |
  | `host` / `remote` | ssh host, in the route (not both, unless `host` names this machine) | `--host`, remote run |
  | `account` | GitHub account login (github only) | the credit gate's account pick |
  | `identity` | `auto\|user\|sp` (Databricks only) | `--as`, the profile's identity |
  | `codexNetwork` | `true`: codex `--tools` `workspace-write` sandbox gets network (inert under `danger-full-access`; top-level ask only) | sealed network |
  | `cloud` | `true`: diff or repo goes to the vendor's cloud (`claude review`, `codex cloud exec`); own login | local |
  | `maxChildren` | Concurrent children per parent; env > overlay > default; 0 = no cap, invalid = default | 8 |
  | `maxTreeTokens` | Known subtree tokens; env > overlay > default; 0 = no cap, invalid = default | 0 |
  | `budgetUSD` | Claude only: number > 0, passed as `--max-budget-usd` | uncapped |
  | `budgetCredits` | Copilot only: integer >= 30, SDK session credit limit | uncapped |
  | `set` / `unset` / `keep` | env name -> value / names / names | env layer |

- `<agent> models` passes through to the vendor CLI; the catalog is `<agent> catalog`.
- **Model precedence** (`agent.ResolveRun`; model merges per key across layers, preflight refuses an unserved one):
  a typed model (`--model`, overlay `model`), then a
  typed `auto`, then the agent's pinned model (record `model` or `<AGENT>_MODEL`), then the task tier: a
  `Tier: fast|standard|deep` line in the brief wins, else its wording picks (audit, security, parser, design: deep;
  typo, rename, move: fast), and a brief the effort classifier rates high never takes the fast tier.
  A pinned `auto` beats a concrete ladder cell exactly as a typed one does, so the label always equals the
  run. A tier the row
  does not serve falls back to the untiered pick with a note. The ranks are seeded estimates: edit
  `internal/catalog/ranks.json`.
  `--model auto` or `configure agents <agent> model=auto` picks the least-recent eligible model on a rotation row
  (Databricks workspace or copilot GitHub login); `auto` rotates models only, never the row. Claude's model never
  rotates: its typed or pinned `auto` runs the brief's tier model (haiku fast, sonnet standard or untiered, opus deep)
  on the row its route names (typed, inherited, pinned, else the session's), and label, `gateway resolve`,
  `ask --dry-run` and the run all show it. Each agent has one history per provider group across dev, qa and prod;
  holds stay per workspace: a pool-pinned pick skips a model where that workspace skips or holds it, and keeps it
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
  Vault agent records store `route`/`fallbackRoute` objects (`configure agents <a> profile=qa` is sugar,
  `route='{…}'` takes the object; legacy profile/account/fallback are read; a copilot record is route
  `{github, account}` plus `fallbackRoute`). Dispatch records write `route`/`ran_route` as objects.
  A model string is a plain id or alias only; the route travels apart: `providers.md` → Routing precedence.
  Own-login `<login>/auto` is refused before launch, naming that login's typed ids.
  Labels, `gateway resolve`, and `ask --dry-run` only peek; `agents label <agent> --run` records and freezes the pick
  as one `--overlay '{"provider":"<provider>","profile":"<p>","model":"<id>"}'` in the `Run:` line. The
  `dispatch-effort` hook does the same for a dispatch whose `Run:` line runs `--model auto` (or no model over an auto
  pin, no `--overlay`, `--spec` or `--provider`): it freezes the pick into the line (model, and profile) and labels the
  bare id, so the run is exactly that model; a frozen pick is typed, so a refusal falls back as a typed model does
  (no 3-re-pick auto recovery). An auto label is the picked id in parentheses, `(<id>): task`;
  a typed or pinned model stays bare.
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
  `COPILOT_OFFLINE=true`, no credits); `--overlay '{"provider":"<provider>","profile":"<p>"}'` crosses profiles.
  The GitHub login is used only
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
