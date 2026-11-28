---
paths:
  - "internal/agent/**"
  - "internal/routing/agentconfig/**"
  - "internal/routing/agentid/**"
  - "internal/driver/**"
  - "internal/dev/setup/install*.go"
  - "agents/**"
  - "commands/**"
  - "workflows/**"
---
# Agents: dispatch, overlay, agent CLIs and authoring

What a session needs to dispatch an agent, change agent code, or write an agent or command file. Roster and
dispatch rules: `CLAUDE.md`. Briefs, relay, concurrency, escalation and test agents:
`skills/subagent-workflows/SKILL.md`. Verbs:
`skills/subagent-workflows/references/agent-verbs.md`; ask flags and lifecycle: `references/cost-routing.md`. Routes,
credentials and the proxy: `providers.md`.

## Commands

- `claude-code agents sync [--agent a] [--dry-run|--check]` projects agents and instructions; install runs it.
- `agents list` shows CLI agents, not the subagent roster; JSON exposes capabilities.
- `agents tree [--watch|--detail|--files]`: sessions, held/free dirty files, leaf notes. `agents watch [--once]`
  lists stale jobs and next actions; hooks run it.
- `agents label <type> [--profile p] [--model m] --task F` prints the resolved description prefix (aliases or full
  ids; Databricks labels use full ids). `review scope --range <r> --caps` checks scope.
- `agents ask --brief FILE|-|'{...}'`: task-only JSON (`prompt`, `own`, `report`, `verify`, `contract`, `action`);
  configuration goes in `--overlay`. An unknown key or bad value exits 2 naming the nearest key. `--batch` lines
  take the same keys plus `id` and `plan_task`. Go runs `verify` once after an answered run with status ok (not for
  remote work) and records it; a failed verify does not change the run's status.
- `agents ask --dry-run` prints agent, route (as `gateway resolve`), hops, overlay and the canonical argv through
  the real resolvers; no record, no model call, no pick stamped; not with `--live`, `--batch`, `--job`.
- `agents ask --overlay '{"agents":<json>}'` defines agents inline (`name`, `description`, `prompt`, `tools`,
  `model`, `max_turns`, `agent`, `profile`, `effort`); `host` with `agents` exits 2.
- `agents ask --loop "<goal>"` (top level only) plans rounds on `--loop-planner` (claude), runs them on the
  `--batch` pool, retries a failed part once on claude (not a typed model or `"fallback":"none"`), keeps state in
  `state/harness/ask/loops/<id>.json`; resume with `--loop-resume <id>`.
- Reviews: `/code-review` or `claude-code codex review [--adversarial] [--base <b>]`.
- Cloud execution needs outermost overlay `{"cloud":true}` and the vendor login: `claude-code claude review
  [--base b|--pr]` runs ultrareview; `claude-code codex cloud exec "<q>" --env <id>` submits. Cloud
  status/list/diff/apply need no opt-in. Vendor passthrough refuses ultrareview/cloud exec; use the harness lane.
- `<agent> models` passes through to the vendor CLI; the catalog is `<agent> catalog`. For codex and copilot,
  `databricks -p dev` (or `--profile dev`) selects the profile; everything after the first `--` goes to the vendor
  CLI untouched (`claude-code copilot -- databricks`).

## Dispatch behavior

- No dispatch or memory job carries a permission policy (R30): tools run whole and unprompted, only `safety-scan`
  judges calls, Claude uses `bypassPermissions`. `--workdir` is not a write boundary.
- Tools default on. Overlay `tools: false` disables them for claude, opencode and pi; copilot's SDK rejects calls,
  gemini receives an instruction only, codex keeps a read-only shell. `noTools` is Claude-only. An allow rule never
  reopens a deny. Details: `skills/subagent-workflows/references/dispatch-policy-details.md` → Tools.
- `ask --format json` envelope: `agent`, `status` (`ok|error|partial|quota|stalled`), `model` (what ran),
  `invoked_model` (what was asked), `answer`, `usage`, `duration_ms`, `fallback*`, `handoff`, `transport`,
  `route`/`requested_route` objects. Isolation: overlay `noPersist` (claude, codex), `debugFile` (claude, gemini).
- `ask --retry <id>` of a run cut off by a deadline, stall, idle or auth-retry runs on claude unless an overlay
  `model`, `agent` or `"fallback":"none"` pins the route or claude itself was cut off; it names the move. A plain
  resubmit of the same agent, model and task within 60 minutes gets the same rule
  (`state/harness/agent/cutoffs.json`, task hashes only); a successful run on the route clears its entry.
- **Caps:** nesting depth 5, live children per parent 8, tokens under the root 20,000,000 (`0` = no cap; invalid
  uses the default). Each resolves env (`HARNESS_DISPATCH_MAX_DEPTH`, `_MAX_CHILDREN`, `_MAX_TREE_TOKENS`) > overlay
  (`maxDepth`, `maxChildren`, `maxTreeTokens`) > default, propagates to descendants, applies to nested dispatches
  only and refuses terminally; a running dispatch is never stopped. With no dispatch id in its env, a dispatch finds
  its parent by walking its parent processes (macOS and Linux), so `env -i` in a leaf does not make a top-level
  dispatch; the route fence is not restored that way. A live top-level `--carry` level is not counted.
- **Memory jobs:** `memory update` (`--dream`) and catch-up run claude on their prompt only, with tools, each hop at
  `"fallback":"none"`: the pin, the other Databricks workspaces, then `magnitude`/`local` when its `/models` answers
  within 2 s (marked lower trust).
- Agents propose memory in reports; the main session writes it. Dispatch attaches recall pointers.

## Overlay

`agents ask --overlay`, `gateway --overlay` or vault env `COWORKER_OVERLAY` take one flat JSON document: schema 2
(omitted/older accepted), a private regular file ≤64 KiB, inline JSON or `-`. A typed unknown key stops with a
named-key error; a stored unknown key warns and is dropped; invalid values stop, except malformed optional caps,
which use defaults. File ownership, env-name refusals, relaunch and `--env`:
`skills/subagent-workflows/references/dispatch-policy-details.md` → Overlay.

- **Source:** ask uses `--overlay`, else the run's `COWORKER_OVERLAY`, else the vault default (top level only);
  the three are never merged. A typed flag overrides an inherited or stored key; settings sit below.
- **Flags and keys are one setting:** every launch and `ask` flag with an overlay key (`--profile`/`--provider`,
  `--model`, `--direct` → `endpoint`, `--keep-env` → `keep`, …) is accepted either way. Both with the same value are
  one setting; with different values the command exits 2 naming the flag. Actions (`--print-env`, `-h`) stay flags.
- **Route unit:** an explicit route names provider and profile together and endpoint travels with it; a partial
  unit is refused (a provider without profiles may be selected alone by flag). The pool pick is overlay profile
  `auto`; `ask --profile auto` and `label` refuse it. A batch-wide flag must agree with a line's overlay.
- **Children:** a dispatch derives a copy of the parent's overlay plus the call's input (`Overlay.Derive`) and never
  writes back. Children take only the table's inherited keys, then their own; shell model variables and kept
  credentials do not cross. A dispatch child is fenced to the leaf's concrete profile within the inherited fence
  (`auto` and session children add none); a closed fence fixes provider and allowed profiles, not endpoint or
  identity. Fallback clears the model when provider/profile changes without an explicit model.
- **Replay:** session state saves route, model, inherited caps, runner and fence (`--resume` → `HopResume`, the
  saved overlay whole, overridden only by typed keys). Job records save route, model, fallback, caps and budgets
  (`--retry`/`--job` → `Record.ReplayOverlay`). Append/resume may replace run restrictions and budgets, never widen
  caps or the fence.
- **Interactive launch:** needs explicit `--overlay`; it skips keys only `ask`/`gateway` use (`fallback`, `cloud`,
  budgets, `tokenCommand`, `codexNetwork`) with a note when typed; the fence still binds at token mint.

The table is the only overlay key catalogue. Schema `both` = launch and dispatch, `dispatch` rejects set values at
launch, `none` rejects presence. Child = inherited by a child (`reset` regenerates). Tighten: `yes` cannot loosen,
`fenced` follows the inherited fence, `if set` caps an explicit value. Acceptance does not promise consumer support.

| Key | Type | Schema | Child | Tighten |
| --- | --- | --- | --- | --- |
| `schema` | `int` | both | reset | no |
| `provider`, `profile` | `string` | both | yes | fenced |
| `identity`, `account`, `endpoint` | `string` | both | yes | no |
| `model`, `effort` | `string` | both | no | no |
| `fallback` | `string` | dispatch | yes | no |
| `runner` | `string` | both | yes | no |
| `host`, `remote` | `string` | both | no | no |
| `providerMode`, `set` | `map[string]string` | both | no | no |
| `unset`, `keep` | `[]string` | both | no | no |
| `tokenCommand` | `*[]string` | dispatch | no | no |
| `maxChildren`, `maxTreeTokens`, `maxDepth` | `*int` | both | yes | yes |
| `codemode` | `string` | both | yes | no |
| `fence` | `*route.Fence` | both | yes | yes |
| `budgetUSD` | `*float64` | dispatch | no | if set |
| `budgetCredits` | `*int` | dispatch | no | if set |
| `codexNetwork`, `cloud` | `bool` | dispatch | no | no |
| `noTools`, `noPersist` | `*bool` | both | true only | yes |
| `tools` | `*bool` | both | false only | yes |
| `timeout` | `string` | both | yes | yes |
| `agent`, `tier` | `string` | both | no | no |
| `agents` | `json.RawMessage` | both | no | no |
| `systemPrompt`, `appendSystemPrompt` | `string` | both | no | no |
| `outputSchema`, `debugFile`, `googleTools` | `string` | both | no | no |
| `native`, `autonomous`, `fleet`, `enableMemory` | `*bool` | both | no | no |
| `file` | `string` | both | no | no |
| `files` | `json.RawMessage` | none | no | no |

Caps take the smaller positive value. Budgets stay unset unless supplied, then cannot exceed the parent's: USD is
Claude-only (>0), credits Copilot-only (integer ≥30). `timeout` is wall time (a positive Go duration, no maximum)
and only shortens. Top-level `codexNetwork` enables network in `workspace-write`; `cloud` uses vendor cloud with its
own login. `file` is launch-config only.

## Model precedence

`agent.ResolveRun` merges models per key across layers; preflight refuses unserved models. Model strings, route
order and fallback values: `providers.md` → Routing precedence.

- **Order:** typed `model` (including `auto`) > agent record (`model` or `<AGENT>_MODEL`) > task tier. A brief's
  `Tier: fast|standard|deep` wins, else wording picks deep for audit/security/parser/design and fast for
  typo/rename/move; high effort never takes fast. An unserved tier uses the untiered pick with a note. Ranks are
  estimates in `internal/assets/catalog/ranks.json`; `auto` ranks the served models for the brief's tier with them
  and, typed or in the record, replaces a concrete ladder cell. `fallback:none` refuses only a concrete typed
  model. Labels show the pick.
- **`auto`** (typed or in the record) rotates the least-recent eligible model on a Databricks workspace or Copilot
  GitHub row, from the served listing or Copilot snapshot only, and never rotates the row. Claude never rotates:
  `auto` runs haiku fast, sonnet standard/untiered, opus deep. History is shared across workspaces per provider
  group; holds are per workspace. `ranks.json` controls `rotate: false`, the tier's capability floor and cost cap,
  and `defer_cost`/`defer_turns` (standard cost 5, every 3 turns). Held models and models probed `no` for tools are
  skipped. Outside rotation rows, or with a cold or stale listing, the row's tier model runs (cold gets a note).
  A fresh tier with nothing runnable steps to standard; no eligible model then refuses, naming the held ids.
  Own-login `<login>/auto` is refused before launch.
- **Refusal recovery:** auto refusals share at most 3 re-picks (429 moves are free), never after text or tools.
  Under fallback none an exhausted pool may move within the provider, never past the inherited fence. A typed
  workspace starts there; a 429 may move it unless fallback none.
- **Peeking:** labels, `gateway resolve`, `dispatch-effort` and `ask --dry-run` agree with execution and move no
  rotation history. A label is always `<id>: task`.
- **Records:** agent records store `route`/`fallbackRoute` objects (`configure agents <a> profile=qa` is sugar).
  Dispatch records write `route`/`ran_route` (the final launched hop) and `attempts` only for multiple hops.

Per agent:

- **claude:** a Responses-only model is refused at launch and by the router; use codex or copilot.
- **opencode** on a Databricks row runs three providers: `<row>` (chat), `<row>-gpt` (Responses), `<row>-claude`
  (Anthropic); an unlisted model is refused at preflight. Its `SERVED` column shows `y` READY, `n` not READY, `?`
  missing or stale. Effort: TUI `ctrl+t`, `opencode run --variant high`, or ask overlay `effort`.
- **copilot** follows the session profile (then its record) through the proxy (`/p/<profile>`, BYOK,
  `COPILOT_OFFLINE=true`, no credits), except a record on its own GitHub row, which decides. The GitHub login is
  used only while that account's verified credits are under 95%, else the Databricks profile, never the other
  GitHub account. Interactive copilot on Databricks gets native `/model` through a per-launch registry and a
  login-free `COPILOT_HOME`; an unregistered model fails closed, `--direct` keeps the single-model route, macOS
  skips the registry. Prompt cap = window − 32k − 10% (`models.PromptTokens`).
- **gemini** runs on `agy`: its Google sign-in, or a Databricks profile through the proxy (`providers.md` →
  Databricks routes). `ask --dirs/--files/--diff` inline tracked files only, never `.env*`, `state/` or keys.
  Overlay `googleTools` `search|url|code` (gemini only; other agents exit 2) turns on Gemini built-ins on a
  Databricks row; preflight refuses a tool the row does not serve (`options.google_tools`; unset = unprobed, a
  warning). agy's own Google login refuses every tool. Code: `internal/routing/googletools/googletools.go`.
- **codex children:** native `spawn_agent` children use the Codex routing cells on a Databricks profile with a
  validated catalog, only when explicitly fresh (`fork_turns="none"` or `fork_context=false`) with no model/effort
  pin or custom type. A codex record `model` or `effort` disables child routing for the session. Its hooks log to
  `state/harness/codex/routing-<session-hash>.jsonl` (0600, no prompts).

## Pi

Pi (`@earendil-works/pi-coding-agent`, pinned 1.6.0, Node 22.19+) runs on a Databricks profile through the proxy with
no login of its own: `agents ask --overlay '{"agent":"pi"}'`. It is the default leaf (Claude in-context on refusal),
write work included. `make` and live checks stay with the main session.

- **Isolation:** a private agent dir under the harness state dir (nothing in `~/.pi`); `--no-context-files
  --no-mcp --no-approve --no-extensions --no-skills --no-prompt-templates --no-themes`, so a Pi brief must stand
  alone: no CLAUDE.md, skills or MCP. Pi has no permission prompts or sandbox; hook overrides are not inherited.
- **Tools:** read, bash, edit, write, grep, find, ls through the guard extension, which calls `hook PreToolUse
  --pi-guard` (`rules/harness/hooks.md` → Pi · plugin). A missing guard or wrong Pi version fails closed. An
  overlay `set` naming Pi's agent dir or a harness-owned name is refused. Only menu ids run or enter auto.
- **Typed actions:** the guard also registers `gate` and `handback`; the model picks the action and arguments and
  Go builds the command. A typed hand-back (status, summary, unfixed) becomes the run's answer.
- **Success:** the exit code proves nothing: require `agent_settled`, last stop reason `stop` and no open tool call.
  Length, abort, unsettled tool use and cut streams fail.
- **Wires** (`internal/driver/pi/config.go`, `pickWire`): Claude → Anthropic; Responses-capable non-Claude,
  non-Grok → Responses; the rest → chat. `effort` maps to `--thinking`. `"endpoint":"direct"` is refused; Pi has no
  interactive launch.
- **Runner:** a roster agent's first hop on its own Databricks route is Pi with that agent's resolved model, then
  its own CLI (`internal/agent/pirunner.go`, `piRunnerHops`). `runner: cli`, model auto, resumed sessions and
  `native: true` keep the own CLI; `--carry`, `host`, `--as`, a vendor session or budget, or `endpoint: direct`
  remove Pi hops. Plain `agent: pi` tries Pi on the workspace, sibling workspaces with the same model, then Claude;
  leaving Databricks needs typed `fallback: all`. Own-login routes (Anthropic, Copilot, Google) are unchanged.
  Rows and docs name the asked agent; Pi is "the pi runner".
- **Local last hop:** after hosted hops fail before answering, one local Qwen hop may run (`magnitude`/`local`,
  `ranks.json` `local.stage: last`, `internal/agent/localhop.go`), one slot, tiers fast and standard only. Fallback
  none, a typed model, native/host/`--as`, a fence, security briefs or a prompt over `local.max_prompt` bar it.
  Fable on Databricks is refused, never replaced by Opus.
- **rtk wrapping:** on for every Pi launch except code mode: stand-ins under `state/harness/rtkwrap/bin` run `rtk
  <cmd>` only when stdout is a pipe and Pi's own tool shell ran the command; any failure runs the real binary.
  Decisions log to `state/harness/rtkwrap/decisions.jsonl`. Windows has no shims. Other agents: `rtk.md`.
- **Code mode** (per call, trial): overlay `"codemode":"off|on|only"` with `tools`, or `CODEMODE` per machine (a
  typed value wins). Pi loads `builtin:codemode` (scripts call the guard's tools in a sandbox; `only` routes every
  tool through scripts); claude takes `on` only on the direct Anthropic route; opencode takes `on`. A run that
  cannot give it runs with code mode off and prints `codemode: off for <agent> (<reason>)`. Code mode never wraps
  with rtk. `"agent":"coworker"` is `"agent":"pi","codemode":"on"`. Use it for read-heavy fan-out (audits, sweeps),
  not single reads or edits.
- **Packages:** the owner pins an exact list: `claude-code configure agents pi packages=<spec>[,…]` where a spec is
  `npm:[@scope/]name@<x.y.z>`, `git:<host>/<owner>/<repo>@<40-hex commit>` or a local path. They install
  credential-free into `state/harness/pi/packages`. A package that ships code is refused unless its exact spec is
  in `packages_approved`; a new version needs a new approval. Only the owner's own session sets either key. A
  package cannot replace a guarded tool and its own tools are blocked. `claude-code clean` (target `pi-packages`)
  removes unlisted slots. **Risk:** approved extension code runs unsandboxed with the user's privileges, outside the
  guard, and local paths are not hashed, so later changes to approved code load unchecked.
- `doctor` rows: `pi route`, `tool pi`, `pi packages`.

## Carried asks

`agents ask --carry --overlay '{"agent":"<a>"}' [ask flags] -- "<task>"` runs the inner ask with identical task
bytes and streams its output, with no model call of its own on success.

- Only codex, copilot, gemini and opencode carry. Refused: pi, claude, personas, `--live`, `--batch`, `--loop`,
  `--loop-resume`, `--waves`, `--retry`, overlay `host`, `--dry-run`, `--env`, `--overlay -`.
- A cutoff (deadline, stall, idle, auth-retry) hands the task and the last 16 KiB of output to Pi to continue; the
  record carries `takeover_of` and the receipt `ask: finished by pi (<model>)`. A typed model plus fallback none
  refuses takeover.
- Long runs: `--background`, then `agents result -f <id>`; `--events` streams child and takeover events.
- A carried child has a direct call's depth, inherits caps and the carrier's fence, and adds no route. Name the
  route in a carried command; do not lean on Pi's Databricks default there.
- Each shell layer wraps only its own commands: carried PATH excludes Pi's shims.

## Slash commands

Owner-only (`disable-model-invocation: true`); each launcher names its dispatch tool.

- `/codex`, `/copilot`, `/gemini`, `/opencode` accept host/provider/profile/model/effort/fallback and a task, and
  run `agents ask --carry --background --overlay '{"agent":"<type>","tools":true}' - < brief` plus supplied keys,
  then `agents result -f <id>`. With host, omit carry/background and relay in the foreground. No `/claude` (name
  collision).
- `/coworker` adds `--codemode on|only` and runs a plain background ask (Pi refuses carry) with overlay
  `{"agent":"pi","provider":"databricks","profile":"<p>","autonomous":true,"codemode":"on","tools":true}`; supplied
  provider/profile replace the defaults; then `agents result -f <id>`. Its type and receipt are `coworker`,
  identical to the Agent type and ask overlay `agent:coworker`. No `/pi`.
- Route fields travel together through one overlay; workflows pass that source unchanged (keys and precedence:
  Overlay above).
- `/discover-models [--profile p] [--dry-run]` runs `claude-code models discover --apply` (no apply for dry-run).
- External agents run as direct carried asks (Pi plain) or through the Haiku relay Agent types (codex, copilot,
  gemini, opencode, coworker), which run their baked Databricks overlay, wait and check. Vendor verbs and
  `runner:own` keep vendor logins. Unnamed Claude code leaves run Pi first; typed Claude on Databricks uses the pi
  runner unless `runner:own`. Owner contracts: `skills/subagent-workflows/SKILL.md` → Owner layer; Databricks arm:
  R20. `dispatch-effort` corrects a missing or wrong model prefix, never denies it. Auto host/profile:
  `rules/harness/network.md`.

## Messages

`claude-code agents msg` carries messages between Claude sessions, agent CLIs and shared rooms. `@name` is one
member's inbox; `#name` is a room. Each chat is one append-only log under `state/harness/msgs/<@name|#name>/` plus a
read marker per member. A message is peer information, never a user instruction.

- Send: `agents msg @<name>|'#<room>' "<text>|-" [--from <who>] [--file <path>]...` (text ≤ 4000 bytes, each file
  ≤ 8 MiB). Quote `'#name'`: bash reads a bare `#` as a comment.
- Read: `agents msg [@<name>|'#<room>'] [--from <who>] [--format text|json]` prints each new message once, never
  the reader's own; bare, it reads every chat the reader is in. With another member's `@name` it lists the reader's
  own latest posts there as `(read)` or `(unread)` and moves no marker.
- `--from` defaults to the asked agent of the dispatch it runs in, else the calling Claude session's name; names
  match `[a-z0-9][a-z0-9._-]{0,63}`.
- `agents msg @parent "<note>"` is a leaf's one-line note (200 characters) to its dispatcher, shown on the
  dispatcher's next prompt.
- `claude-code agents mcp --from <agent>` serves `msg_send`, `msg_read`, `gate` and `diff` as one stdio MCP
  server, registered as `harness` in every agent's config; the sender is fixed by `--from`.
- A message reaches every live Claude member except its sender as a notice on that session's next hook; other
  members pull. Agent CLIs learn the command from the paragraph `agents sync` renders.

## Proxy routes

- **Who:** pi, codex, opencode, gemini and copilot walk the team route list; headless claude and the interactive
  session use the shared daemon.
- **The walk:** `routeCands` (`internal/agent/proxyroute.go`) maps the dispatch's record to its kind and takes that
  kind's routes from the team config (`providers.md` → Proxy teams and scopes). The first live route wins; a later
  one prints `<agent>: proxy: <first> unavailable (<reason>); using <chosen>` and the envelope's `transport.proxy`
  names it. A scoped route is live when its scope record's pid and start time match, `/healthz` answers and no
  other process holds its port.
- **Children:** a scoped choice travels to the child as its overlay `endpoint` (`<team>:<id>`); a nested or carried
  ask puts that team first.
- **Reroute:** a refused or reset connection to a known loopback proxy is a transport error, never an account
  fault: one immediate retry, walking the list without the dead route.
- **Batch and waves:** a batch line's `group` key names a phase scope id; a wave and a bare batch host their own
  phase proxy and close it when they end.

## Config writes and parity

- **Launchers:** `bin/<agent>` is the unrouted CLI, except `bin/claude`, the `claude-code claude --launcher`
  wrapper (native Claude is `~/.local/bin/claude`); routed launches use `claude-code <agent>`. The wrapper applies
  the session profile overlay and embedded autoMode rules. A failed pinned route fails closed; only plain Anthropic
  passthrough may fall back to native Claude without harness settings.
- Agent CLI configs are merged, never replaced (`$CODEX_HOME/config.toml`, `~/.gemini/antigravity-cli/settings.json`,
  Codex/Copilot hooks); each overwrite keeps 5 timestamped backups and `configure revert [--agent a]` steps back.
- opencode's `~/.config/opencode/` gets only the theme file and synced agent files; model, MCP and theme config
  come from a tracked overlay injected at launch (`internal/assets/agentconfig/template/`).
- Codex pins (effort, verbosity, `config_route`) live in the vault codex record and apply to
  `$CODEX_HOME/config.toml`; provider config, hook trust and unknown native keys are preserved; an unavailable
  vault key aborts changed writes. The file's route follows the session profile unless `config_route` pins it
  (`configure codex`); a changed provider, model or catalog prints a restart notice.
- **Projections:** `agents sync` renders under `state/projections/<agent>/` and installs into agent homes. Never
  put AGENTS.md at this root.
- **Skills:** `agents sync` links `skills/<name>/SKILL.md` into `~/.agents/skills`, copilot's `~/.copilot/skills`
  and agy's `~/.gemini/antigravity-cli/skills`. `skills/synced/**` gets no link (opencode reads `~/.claude/skills`).
- **Portable core:** `CLAUDE.md` plus unscoped `rules/standards/*.md` (`project.PortableCore`); headless and
  interactive runs load the same core.

| Agent | Reads the core from |
| --- | --- |
| codex | `~/.codex/AGENTS.md` (rendered by sync) |
| gemini (agy) | `~/.gemini/GEMINI.md` (rendered by sync) |
| copilot interactive | `~/.copilot/copilot-instructions.md` |
| copilot `ask` (SDK) | appended system text plus instruction, skill and agent dirs (`internal/driver/copilot/sdk/sdk.go`) |
| opencode | `~/.claude/CLAUDE.md`; its own `AGENTS.md` would replace it, so sync writes none |

- Copilot hooks run natively from `~/.copilot/hooks/*.json`, which the SDK session loads too, so no SDK callbacks
  are registered (each guard would run twice).
- `doctor` row `agent flags <agent>` checks each CLI's declared flags against its help (`ok`, `drift`, `skipped`);
  report-only, for claude, codex, copilot, gemini, opencode and pi.

## Authoring: agent, command, skill or brief

- Agent = reusable role; skill = method; brief = task. A one-off role is a Claude brief, never a file. Split for
  different expertise, tools, authority or reused shapes; merge wording-only variants. A routing-only role is a
  direct carried ask (Pi plain), except the four external workers; saved stages use Claude.
- `agents/<name>.md`: flat, one definition, frontmatter `name` equals the file stem; the description says when to
  use it. Sections as needed: Role, Invocation, Owns, Knows, Verbs, Hands off to, Procedure, Report contract, Don't.
  Knows lists references and when to read them; nothing loads upfront. List only needed tools.
- `commands/<name>.md`: a short launcher that reads `$ARGUMENTS`, runs the agent or script and relays the report.
  Procedures go in agents or skills, skill shell in `skills/<name>/scripts/`, other logic in Go verbs or Make.
- Skills start from `docs/templates/skill.md`: one procedure home, heavy material in references.
- Models: name an alias (`providers.md` → Model selection). Leaves default to sonnet; an unpinned Claude leaf uses
  the tier model (haiku/sonnet/opus); Plan inherits; relay types use haiku. Raise one call with the Agent `model`
  parameter. File `inherit` means the session model with no hook pin; a call's `model:inherit` is unset.
  Frontmatter also takes effort, maxTurns, isolation:worktree, background, memory; check unknowns with
  `claude --debug`.
- A new CLI is registered in `internal/routing/agentid/agentid.go` plus its vendor `agent.Register` row; subagent
  types live in `agents/`. Prose instructs, hooks enforce; `dispatch-stop` reports a running background job
  without holding the leaf. Shapes and enforcers: `skills/subagent-workflows/references/patterns.md`.
- Use adaptive thinking, never manual budgets. Cache stable large prefixes with `cache_control`; keep per-call text
  out of them.
- A moved or renamed agent or command: search the old name and fix every caller in the same change.
