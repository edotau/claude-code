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

- Permissions (R30): no agent or memory job carries a permission policy; every dispatch runs with its tools whole,
  and only `safety-scan` (`settings.json` deny rules, the AI-attribution block) judges its calls.
- `ask --format json` envelope: `agent`, `status` (`ok|error|partial|quota|stalled`), `model`, `answer`, `usage`,
  `duration_ms`, `fallback*`, `handoff`, `transport`, `route`/`requested_route` objects (`provider` kept one release);
  `invoked_model` is the plain model asked (`auto` or the selector; the profile is the route's) and `model` the model
  that ran. Isolation: `--no-persist` (claude, codex), `--debug-file <path>`
  (claude, gemini).
- `ask --retry <id>` of a run cut off by a deadline, stall, idle or auth-retry runs on claude unless a `--model`,
  `--agent` or `--fallback none` pins the route or claude itself was cut off; it keeps the route and names the move.
  A plain resubmit of the same agent, model and task within 60 minutes of such a cutoff gets the same rule, from
  `state/harness/agent/cutoffs.json` (task hashes only); a successful run on the route clears its entry.
- `agents ask --loop "<goal>"` (top level only) plans rounds of parts on `--loop-planner` (claude), runs
  them on the `--batch` pool, retries a failed part once on claude (not a typed model or `--fallback none`) and keeps
  state in `state/harness/ask/loops/<id>.json`: exit 0 done, 1 `needs_main`, 4 cap or budget, `--loop-resume <id>`.
- Tools are on by default. With `--tools=false`, claude and opencode launch no tools, copilot's SDK rejects every
  request, gemini is only instructed not to use them, codex keeps a read-only sandbox shell. `--no-tools` is claude
  only (others exit 2). An allow rule never reopens a deny; claude `--tools` off Anthropic gates only explicit
  rules. Grants and gates:
  `skills/subagent-workflows/references/dispatch-policy-details.md` → Tools.
- **pi** (npm `@earendil-works/pi-coding-agent`, MIT, pinned 1.6.0, Node 22.19+) runs by
  `agents ask --agent pi`, on a Databricks profile through the loopback proxy, with no own login. It runs the models on
  the profile's Pi menu: an id not on it is refused ("not on the menu") and never enters the auto pool. With
  `--tools=false` it runs with no tools. Any tools run runs Pi with `--no-builtin-tools` plus the harness's guard
  extension (the first `-e`, run dir, 0600), whose wrapped tools are the only ones Pi has: each call is judged by
  `hook PreToolUse --pi-guard` (path normalized as Pi does and judged as typed and as the disk spells it,
  `safety-scan`) and fails closed (a guard that fails to load leaves Pi no tools; it refuses any
  Pi but the pinned one); a cancelled or timed-out run gets SIGTERM, a 2 s grace, then every descendant is killed
  (Pi's bash groups included), and the idle watch is held while any tool call runs; Pi proves it loaded by writing the
  run's nonce, checked on the first stream event, else the run is killed and refused ("pi guard did not load; run
  refused"). Bare `--tools` gives read, bash, edit, write, grep, find and ls (never `powershell`: unmapped, blocked);
  rule tool names match in any case (`write` is `Write`). An `--env` naming `PI_CODING_AGENT_DIR`, `HARNESS_PI_*` or
  `HARNESS_HOOK_*` is refused. Pi has no permission
  prompts or sandbox of its own. A dispatch passes `--no-context-files` (no CLAUDE.md or AGENTS.md), `--no-mcp` and,
  so a repo's own `.pi` never runs code with your privileges, `--no-approve --no-extensions --no-skills
  --no-prompt-templates --no-themes`; `--effort` maps to `--thinking` (minimal to max); `--model auto` and tier
  aliases follow the other agents' rules. Wire routing is per model (`internal/pi/config.go` `pickWire`): Claude ids
  on Anthropic Messages, any non-Claude, non-Grok model that serves Responses on the Responses wire, everything else
  and Grok on chat completions; a model whose wire has no route on the profile is dropped from the menu. Pi exits 0
  even when a request fails, so the harness judges the run from its JSON event stream: success needs `agent_settled`,
  a last message stopped `stop` and no open tool call (`length`, `aborted`, `toolUse` left unsettled, unknown stop
  reasons and cut-off streams fail). Pi is the default leaf route (claude in-context on a Pi refusal) and
  may be a fallback for write work. Its run carries no permission policy (R30), whoever the caller is; bash is
  judged by `safety-scan`. `make` and the live check stay with the main session. An overlay setting `HARNESS_HOOK_*` is
  refused too, and Pi's process never inherits one. The tool-name
  mapping and the guard's modules: `/Users/edotau/.claude/rules/harness/hooks.md` → Pi · plugin.
- **pi runner** — an ask of any roster agent except pi whose first hop is its own Databricks route runs through Pi
  first, on the model the agent resolves there; the asked agent's own CLI is the later hop. `runner: own` in the
  overlay opts a dispatch out. A plain `--agent pi` ask with no fallback mode set (overlay, env, vault) follows
  `agents`: pi on the workspace, pi on the other workspaces (same model; not under a rotation's drop or a
  confinement), then claude on that Databricks workspace (`withPiClaudeHop`, `withPiSiblings`).
  First-party Anthropic, Copilot and Google routes are unchanged, and an auto pick,
  `--fallback none`, a resumed session and `--native` stay on the own CLI, and an explicit profile pin keeps the Pi
  hops on that workspace (`internal/agent/route.go` `withPiRunner`, `piRunnerBarred`, `staysOnWorkspace`).
  A request Pi cannot carry (`--carry`, `--host`, `--as`, a vendor session, `--direct`, a vendor
  budget) drops the Pi hops (`runnerLoses`). Measured on one one-line task: about 3.3k tokens through Pi against 61k
  on the claude CLI. Hop order: Pi on the asked profile, Pi on the other Databricks profiles, then the agent's own CLI;
  hops off Databricks only under a typed `--fallback all`. An overflow refusal re-picks once by context size under
  `all` or `agents` (a typed model is kept, not replaced); a policy refusal and
  `x-should-retry` are typed error classes. Fable is refused on Databricks by name, never run as Opus;
  among non-vendor families only kimi, grok and qwen are picked automatically, the others run by name.
  A brief for a Pi hop must stand alone (no CLAUDE.md, skills or MCP).
- **pi typed actions** — Pi's guard extension also registers `gate` and `handback`: the model picks the action and its
  typed arguments and Go builds the command. A typed hand-back (status, summary, unfixed) becomes the run's answer and
  its footer line (`internal/pi/stream.go` `handbackStatuses`). A spec's `own`, `report` and `verify` keys on
  `agents ask --spec` wrap the brief; Go runs `verify` once after an answered run with status ok (not for remote work)
  and records it as the result's `verify` object; a failed verify does not change the run's status.
  The record carries per-tool stats.
- **pi prompt** — a dispatch passes its prompt after `--` (a prompt like `-a` stays text; a leading `@` gets a space);
  `--direct` is refused, and Pi has no interactive launch.
- **pi rtk wrapping** — always on, except a code-mode run (`--codemode on|only`, or `CODEMODE` resolving to on;
  `--codemode off` wraps; Pi sets `HARNESS_CODEMODE` for it): every Pi launch puts `state/harness/rtkwrap/bin`
  first on PATH and, with code mode off, sets `BASH_ENV` to its `.bash_env` (an EXIT trap that stops bash exec'ing
  a line's last command, sourcing the user's own `BASH_ENV` first). Its stand-ins (`bin/.rtkwrap`, one symlink per
  rtk command) run `rtk <cmd>` only when stdout is a pipe and Pi's own tool shell ran the command.
  Substitutions, env changes, `git commit`, a command piped into another program, `ls -a`/`-A` and the hook's vetoes
  stay raw, and any failure runs the real binary.
  Each call appends a name-and-verdict line to `state/harness/rtkwrap/decisions.jsonl`; there is no switch (a
  missing `bin/.rtkwrap` launches unshimmed with a notice). Windows has no shims.
- **rtk for the other agents** — `rtk-rewrite` in the codex and opencode hooks rewrites every eligible command, never
  while `HARNESS_CODEMODE` is set. Copilot's SDK shell tool rewrites through the same function. agy cannot rewrite
  (it judges its own rules on the rewritten text, so `rtk <cmd>` escapes them) and gets awareness text only.
  `claude-code doctor` shows one rtk row per agent.
- **pi code mode (trial)** — `agents ask --agent pi --tools --codemode off|on|only`, or `CODEMODE=off|on|only` per
  machine (a typed flag wins, so `off` beats the setting; ignored without `--tools`), loads `builtin:codemode`. The
  model writes a script that calls the guard's tools in a QuickJS sandbox; `only` routes every tool through scripts.
  The guard allows that one tool only from `builtin:codemode`, and nested calls still pass `safety-scan`. Code mode
  never wraps (see Pi carrier and experiments, rule 4).
  Plan: `/Users/edotau/.claude/plans/2026-11-22-pi-codemode-trial.md`.
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
- pi compaction and retries are typed `progress`/`retry` marks in the stream log (not in recorded events); the status
  line reads `compacting 12s` or `retrying 4s` (the announced wait) instead of `quiet`, until the next log line.
- The wave owned-files gate covers pi, and `doctor` has the rows
  `pi route`, `tool pi` and `pi packages`.
- `agents ask --agents <json|file>` defines agents inline (`name`, `description`, `prompt`, `tools`, `model`,
  `max_turns`, `agent`, `profile`, `effort`); `--agent` also takes an `--agents` key. With `--batch`, a line's `agent`
  takes the same selector; `--host` with `--agents` exits 2.
- `agents ask --spec FILE|-|'{...}'`: one JSON object whose keys are ask's own flag names (generated from its FlagSet;
  `prompt` is the task; `overlay`, `agents`, `output-schema` also take an object). An unknown key or bad value exits 2
  naming the nearest key; typed flags beat the spec, typed list flags add to it. `--batch` lines use the same keys
  (`id`, `plan_task` extra; a batch-wide key on a line exits naming it). `--dry-run` prints agent, route (as `gateway
  resolve`), hops, overlay and the canonical argv through the real resolvers; it saves no record and
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
  `codexNetwork` and `cloud` are refused naming the key. Held-job hints print `--overlay '<object>'`; batch lines take
  an `overlay` key.

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
  | `maxTreeTokens` | Known subtree tokens; env > overlay > default; 0 = no cap, invalid = default | 20000000 |
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
  `internal/assets/files/catalog/ranks.json`.
  `--model auto` or `configure agents <agent> model=auto` picks the least-recent eligible model on a rotation row
  (Databricks workspace or copilot GitHub login); `auto` rotates models only, never the row. Claude's model never
  rotates: its typed or pinned `auto` runs the brief's tier model (haiku fast, sonnet standard or untiered, opus deep)
  on the row its route names (typed, inherited, pinned, else the session's), and label, `gateway resolve`,
  `ask --dry-run` and the run all show it. Each agent has one history per provider group across dev, qa and prod;
  holds stay per workspace: a pool-pinned pick skips a model where that workspace skips or holds it, and keeps it
  while another workspace serves it. The pool input is the served listing (a GitHub login: its Copilot catalog
  snapshot) alone; there is no built-in seed pool.
  Rotation admits only rows not marked `"rotate": false` in `internal/assets/files/catalog/ranks.json`; typed, those
  still run. The pool admits only models the agent can run within the tier's `rotation` capability floor and cost cap in
  `internal/assets/files/catalog/ranks.json` (unranked models skip both where the tier admits them); a tier's
  `defer_cost`/`defer_turns` make a model that dear take one of every `defer_turns` of its turns (standard: cost 5,
  3 turns; a lone or only-remaining dear model still runs). The vault's
  `discovery-models` allowlist only narrows it; held models and, under `--tools`, models probed tools `no` are skipped.
  Outside a rotation row, auto runs the row's tier model (no listing to rank). A cold (missing or stale)
  listing runs the row's tier model with a note; a fresh listing with nothing runnable in the brief's tier and
  nothing held there steps to the standard tier, and only nothing eligible then exits naming held ids, without
  launching; unwritable pick state errors. An untyped, unpinned model keeps the ranked pick.
  Refused auto picks share at most 3 re-picks per ask, including a move after an exhausted pool (a 429 move is
  free); no re-pick follows streamed text or tool execution. Under `--fallback none` an auto-picked workspace
  whose pool is exhausted moves to another workspace of the same provider, never past an inherited `--profile`. A
  typed `--profile <workspace>` is a start, not a lock: a 429 moves it to the next workspace unless `--fallback none`.
  Vault agent records store `route`/`fallbackRoute` objects (`configure agents <a> profile=qa` is sugar,
  `route='{…}'` takes the object; legacy profile/account/fallback are read; a copilot record is route
  `{github, account}` plus `fallbackRoute`). Dispatch records write `route`/`ran_route` as objects.
  A model string is a plain id or alias only; the route travels apart: `providers.md` → Routing precedence.
  Own-login `<login>/auto` is refused before launch, naming that login's typed ids.
  Labels, `gateway resolve`, `dispatch-effort`, and `ask --dry-run` only peek.
  Every label is the bare id, `<id>: task`, whether typed, pinned or picked; a retired `(<id>)` head is rewritten to it.
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

## Pi carrier and experiments

1. **Carrier (D1).** `agents ask --carry --agent <a> [ask flags] -- "<task>"` (spec key `carry`) builds the inner
   ask line itself (its typed flags minus `--carry`/`--background`) and runs it as a plain child process: the task
   goes in on stdin byte-identical, output streams through, and no model runs on the happy path. Only an agent whose
   roster row sets `CLI.Carry` is carried (codex, copilot, gemini, opencode); pi, claude, a persona, `--live`,
   `--batch`, `--loop`, `--waves`, `--retry`, `--host`, `--dry-run`, `--env` and `--overlay -` are refused.
   Plan: `/Users/edotau/.claude/plans/2026-11-22-pi-carrier.md`.
   - **Takeover.** A child cut off (deadline, stall, idle, auth-retry, or a partial/stalled record) is finished by
     `agents ask --agent pi` under the inner line's tools tier, `--workdir` and overlay caps, on a fixed brief: the
     task, the last 16 KiB of output, "continue and finish; say what
     you changed". Its record has `takeover_of: <child id>`; the receipt prints the child's, then
     `ask: finished by pi (<model>)`. `--fallback none` with a typed model refuses takeover and says so; an `ask`
     rule refuses it too (Pi has no ask).
   - **Long runs.** Add `--background`, then `claude-code agents result -f <id>`: the `--job` child runs the carrier.
     `result -f --events <id>` streams the child's and a takeover's events, each with `via: <child id>`, then the
     carrier's `result`.
2. **The mark (D2).** The outer record has `request.carry: true` and is the parent of the inner run (and of a takeover).
   - Its child has a direct call's depth; the exemption needs a running top-level `carry` record whose live process
     is the child's ancestor.
   - The child inherits the carrier's token caps and any route fence (`COWORKER_OVERLAY`) the carrier
     itself inherited; the carrier adds no route of its own (it runs before ask exports one).
   - Every other nested dispatch, a plain `--agent pi` run's included, stays fenced to its parent's route.
3. **Each shell layer wraps only its own commands.** The rtk stand-ins act in Pi's own tool shell. A carried
   agent's PATH has no shim dir, and the `claude-code agents ask` line between layers is never wrapped.
4. **Code mode and wrapping.** One gate decides from the environment, with no judgment:
   - Code mode off (unset or `--codemode off`) → wrap.
   - Code mode on → never wrap. A script's `rtk <cmd>` or `rtk bypass <cmd>` has the prefix stripped, so the script
     parses exact output; rtk-only verbs (`read`, `recall`, `gain`) run as typed.
   - A record names one `experiment`: `codemode`, `rtk-wrap` (a bash tool, off Windows) or `none`.
5. **When to use code mode (`--codemode on|only`, `off` for one call, or `CODEMODE` per machine):** read-heavy
   work that fans out (audits, sweeps, many-file investigations, a script driving several `agents ask` calls). Not
   single reads or edits: the first runs cost more tokens there.
6. **Turning trials on.** Every switch is recorded in its plan with the numbers behind it.
   - rtk wrapping is on: review `state/harness/rtkwrap/decisions.jsonl` for a wrap on program-parsed output.
   - Code mode: per call until its trial passes, then `CODEMODE` per machine.
7. **Don't** lean on Pi's Databricks route on purpose in a carried command (name the route);
   a leaf's default Pi route is intended.

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

- **copilot SDK vs interactive.** Interactive stays `--allow-all`. `ask --agent copilot` carries no policy;
  hooks run natively from `~/.copilot/hooks/*.json`, which the SDK session loads too, so no SDK callbacks are
  registered (each guard would run twice).
- **Fake vendor binaries.** `HARNESS_AGENT_BIN_DIR` makes `paths.AgentBin` read that directory instead of `BinDir()`;
  real vault pins still route.
- **Dispatch depth.** `HARNESS_DISPATCH_MAX_DEPTH` caps nesting (default 5; 0 = no cap; invalid uses default); a
  live top-level `--carry` dispatch's level is not counted, so its child has a direct call's depth.
  `HARNESS_DISPATCH_MAX_CHILDREN` caps live children per parent (default 8; 0 = no cap; invalid uses default).
  `HARNESS_DISPATCH_MAX_TREE_TOKENS` caps tokens under the root (default 20,000,000; 0 = no cap; unknown usage counts 0;
  invalid uses default; it only refuses new nested dispatches, a running one is never stopped). With no
  `HARNESS_DISPATCH_ID` in its env, a dispatch walks its parent processes once: the nearest ancestor running
  `claude-code` whose pid and start time match a running record becomes the parent, and that record's depth
  and children/tree-token caps apply, so `env -i` inside a leaf no longer makes a top-level dispatch
  (macOS and Linux; the route fence is not restored). Both resolve env >
  overlay (`maxChildren` / `maxTreeTokens`) > default, propagate to descendants, apply to nested dispatches only, and
  refuse terminally.
- **No policy (R30).** No dispatch carries a permission policy: `--tools` gives the agent its tools whole
  (claude runs `bypassPermissions`), `--tools=false` turns them off, and only `safety-scan` (settings.json deny rules
  and the AI-attribution block) judges calls. `--workdir` is no write boundary for any agent.
- **Memory jobs.** `memory update`/`dream`/catch-up run claude on their prompt only, with tools for a known job kind
  (`memory.JobTools`) and no fallback to another vendor.

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
