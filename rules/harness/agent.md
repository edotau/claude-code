---
paths:
  - "internal/agent/**"
  - "internal/routing/agentconfig/**"
  - "internal/driver/copilot/**"
  - "internal/dev/setup/install*.go"
---
# Agent CLIs — codex, copilot, opencode, gemini, pi

## Config writes

- Agent CLI configs are merged, never replaced (`~/.codex/config.toml`, `~/.gemini/antigravity-cli/settings.json`,
  Codex/Copilot hooks); keys are referenced by env var and exported only by a routed launch. Each overwrite keeps 5
  timestamped backups; `configure revert [--agent a]` steps back one.
- pi runs from a harness-generated private agent dir (`PI_CODING_AGENT_DIR` under the harness state dir); nothing is
  written to `~/.pi`.
- opencode's `~/.config/opencode/` gets only the theme file and `agents sync`'s agent files; model/MCP/theme config
  come from a tracked overlay injected at launch (`OPENCODE_CONFIG` / `OPENCODE_TUI_CONFIG`,
  `internal/routing/agentconfig/template/`).
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

- No dispatch or memory job carries a permission policy (R30). Tools run whole and unprompted; only `safety-scan`
  (`settings.json` deny rules, the AI-attribution block) judges calls. Claude uses `bypassPermissions`.
  `--workdir` is not a write boundary.
- `ask --format json` envelope: `agent`, `status` (`ok|error|partial|quota|stalled`), `model`, `answer`, `usage`,
  `duration_ms`, `fallback*`, `handoff`, `transport`, `route`/`requested_route` objects (`provider` kept one release);
  `invoked_model` is the plain model asked (`auto` or the selector; the profile is the route's) and `model` the model
  that ran. Isolation: overlay `noPersist` (claude, codex), `debugFile` `<path>`
  (claude, gemini).
- `ask --retry <id>` of a run cut off by a deadline, stall, idle or auth-retry runs on claude unless an overlay `model`,
  `agent` or `"fallback":"none"` pins the route or claude itself was cut off; it keeps the route and names the move.
  A plain resubmit of the same agent, model and task within 60 minutes of such a cutoff gets the same rule, from
  `state/harness/agent/cutoffs.json` (task hashes only); a successful run on the route clears its entry.
- `agents ask --loop "<goal>"` (top level only) plans rounds of parts on `--loop-planner` (claude), runs
  them on the `--batch` pool, retries a failed part once on claude (not a typed model or `"fallback":"none"`) and keeps
  state in `state/harness/ask/loops/<id>.json`; resume with `--loop-resume <id>`.
- Tools default on. Overlay `tools: false` disables them for claude, opencode and pi; copilot's SDK rejects calls,
  gemini receives an instruction only, and codex keeps a read-only shell. `noTools` is Claude-only.
  An allow rule never reopens a deny.
  Details: `skills/subagent-workflows/references/dispatch-policy-details.md` → Tools.

### pi

- Pi (`@earendil-works/pi-coding-agent`, MIT, pinned 1.6.0, Node 22.19+) uses a Databricks profile through the proxy,
  without its own login: `agents ask --overlay '{"agent":"pi"}'`. Only menu ids run or enter auto selection.
  Tools-off runs have none; tools-on uses `--no-builtin-tools` and the guard extension first (`-e`, run dir, 0600).
  Its wrappers call `hook PreToolUse --pi-guard`, normalizing paths as Pi does and checking typed and disk spellings
  through `safety-scan`. Missing guard or wrong Pi version fails closed; the guard writes a nonce checked on the
  first stream event, else the run is killed. Cancellation/timeouts send SIGTERM, wait 2 s, then kill all descendants,
  including bash groups; active tools hold the idle watch. Tools are read, bash, edit, write, grep, find and ls;
  `powershell` is unmapped and blocked.
  Rule tool names match in any case (`write` is `Write`). An overlay `set` naming `PI_CODING_AGENT_DIR`, `HARNESS_PI_*`
  or `HARNESS_HOOK_*` is refused. Pi has no permission prompts or sandbox of its own. Dispatch passes
  `--no-context-files` (no CLAUDE.md or AGENTS.md) and `--no-mcp`;
  repo code is disabled with `--no-approve --no-extensions --no-skills --no-prompt-templates --no-themes`.
  `effort` maps to `--thinking` (minimal–max); auto and tier aliases follow Model precedence below.
  Wires (`internal/driver/pi/config.go`, `pickWire`): Claude uses Anthropic; Responses-capable non-Claude/non-Grok uses
  Responses; the rest, including Grok, uses chat. Missing profile routes remove models from the menu.
  Each wire uses `http://127.0.0.1:18765/p/<profile>` with a session token exported at launch, never on disk.
  Pi's exit code does not establish success: require `agent_settled`, last stop reason `stop`, and no open tool call.
  Length, abort, unsettled tool use, unknown reasons and cut streams fail. Pi is the default leaf (Claude in-context
  on refusal), including write work. `make` and live checks stay with the main session; hook overrides are not
  inherited. Guard mapping: `rules/harness/hooks.md` → Pi · plugin.

### pi runner

- A roster agent's first hop on its own Databricks route uses Pi with that agent's resolved model, then its own CLI.
  `runner: cli`, model auto, resumed sessions and `native: true` keep the own CLI; workspace auto and fallback none
  keep Pi first. Profile pins keep the workspace. Anthropic, Copilot and Google own-login routes are unchanged.
- Plain `agent: pi` without overlay/env/vault fallback uses `agents`: Pi on the workspace, then sibling workspaces
  with the same model (unless a rotation drop or confinement bars them), then Claude on that workspace.
  Other Pi-first asks try Pi on the asked workspace, sibling workspaces, then the asked CLI; leaving Databricks
  requires typed `fallback: all`. Planning rejects invalid policy/overlay/fence; I/O failures remain runtime errors.
  `--carry`, `host`, `--as`, vendor session/budget or `endpoint: direct` removes Pi hops (`runnerLoses`).
- After hosted hops fail before answering, one local Qwen Pi hop may run (`magnitude`/`local`, `ranks.json`
  `local.stage: last`, `internal/agent/localhop.go`). It has one slot with wait charged to timeout. Fallback none,
  typed pin/model/backend, native/host/`--as`, command-line fence, excluded `local.tiers`, security briefs or prompts
  over `local.max_prompt` bar it. Overflow re-picks once by context under all/agents without replacing a typed model;
  policy refusal and `x-should-retry` are typed error classes. Fable on Databricks is refused, never replaced by Opus.
  Of non-vendor families, only kimi, grok and qwen auto-pick; others need a name. Pi briefs must stand alone:
  no CLAUDE.md, skills or MCP. Implementation: `internal/agent/pirunner.go`, `withPiRunner`, `staysOnWorkspace`.
- **pi typed actions** — Pi's guard extension also registers `gate` and `handback`: the model picks the action and its
  typed arguments and Go builds the command. A typed hand-back (status, summary, unfixed) becomes the run's answer and
  its footer line (`internal/driver/pi/stream.go` `handbackStatuses`). A brief's `own`, `report` and `verify` keys on
  `agents ask --brief` wrap the brief; Go runs `verify` once after an answered run with status ok (not for remote work)
  and records it as the result's `verify` object; a failed verify does not change the run's status.
  The record carries per-tool stats.
- **pi prompt** — a dispatch passes its prompt after `--` (a prompt like `-a` stays text; a leading `@` gets a space);
  `"endpoint":"direct"` is refused, and Pi has no interactive launch.
- **pi rtk wrapping** — always on, except a code-mode run (overlay `codemode` `on|only`, or `CODEMODE` resolving to
  on; `codemode` `off` wraps; Pi sets `CODEMODE=on` for it): every Pi launch puts `state/harness/rtkwrap/bin`
  first on PATH and, with code mode off, sets `BASH_ENV` to its `.bash_env` (an EXIT trap that stops bash exec'ing
  a line's last command, sourcing the user's own `BASH_ENV` first). Its stand-ins (`bin/.rtkwrap`, one symlink per
  rtk command) run `rtk <cmd>` only when stdout is a pipe and Pi's own tool shell ran the command.
  Substitutions, env changes, `git commit`, a command piped into another program, `ls -a`/`-A` and the hook's vetoes
  stay raw, and any failure runs the real binary.
  Each call appends a name-and-verdict line to `state/harness/rtkwrap/decisions.jsonl`; there is no switch (a
  missing `bin/.rtkwrap` launches unshimmed with a notice). Windows has no shims.
- Other agents' wrapping and code-mode exceptions: `rules/standards/rtk.md`.

### pi code mode

- **code mode (trial, per agent)** — `agents ask --overlay '{"codemode":"off|on|only","tools":true}'`, or
  `CODEMODE=off|on|only` per machine (a typed flag wins, so `off` beats the setting; ignored without `tools`). Each
  agent with a row (`agentid.CodeModeAgents()`: pi, claude, opencode) turns it on its own way: pi loads
  `builtin:codemode`, where the model writes a script that calls the guard's tools in a QuickJS sandbox, and `only`
  routes every tool through scripts; claude takes `on` only on the direct Anthropic route (overlay
  `"provider":"anthropic","profile":"auto"`),
  via its `CLAUDE_CODE_EXPERIMENTAL_CODE_MODE` in the `--settings` env block; opencode takes `on` via
  `OPENCODE_EXPERIMENTAL_CODE_MODE`. A run that cannot give code mode (no row, `only` where it is not offered, claude
  off the direct route, or `tools` off) runs with code mode off and prints `codemode: off for <agent> (<reason>)`,
  recorded as `codemode_reason`; only a misspelled value exits 2. The guard allows pi's one tool only from
  `builtin:codemode`, and nested calls still pass `safety-scan`. Code mode never wraps (see Pi carrier and
  experiments, rule 4). The type `coworker` means Pi in code mode (`"agent":"coworker"` is
  `"agent":"pi","codemode":"on"`): `CodeModeRow.TypedByMode` on Pi's row
  (`/Users/edotau/.claude/internal/routing/agentid/agentid.go`) types a Pi code-mode run `coworker`
  (receipt `coworker`) and a Pi run with code mode off by its model (`claude`, runner `pi`).

### pi packages

- Packages do not change routing. The owner pins an exact list in the Pi record:
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
- **pi package risk:** extension code runs unsandboxed inside Pi with the user's privileges, outside the guard.
  It can execute on load, patch the guard, read env and use the network. Review checks only manifest, shipped code
  and exact version; owner approval alone controls behavior. Local paths are not hashed: launch rechecks newly
  added code, but changes to approved code, including `/reload` during a run, load unchecked.
- pi compaction and retries are typed `progress`/`retry` marks in the stream log (not in recorded events); the status
  line reads `compacting 12s` or `retrying 4s` (the announced wait) instead of `quiet`, until the next log line.
- The wave owned-files gate covers pi, and `doctor` has the rows
  `pi route`, `tool pi` and `pi packages`.
- `agents ask --overlay '{"agents":<json>}'` defines agents inline (`name`, `description`, `prompt`, `tools`,
  `model`, `max_turns`, `agent`, `profile`, `effort`); `agent` also takes an `agents` key. With `--batch`, a line's
  `agent` takes the same selector; `host` with `agents` exits 2.
- `agents ask --brief FILE|-|'{...}'`: task-only JSON (`prompt`, `own`, `report`, `verify`, `contract`, `action`);
  configuration goes in `--overlay`. An unknown key or bad value exits 2 naming the nearest key. `--batch` lines take
  the same keys plus `id` and `plan_task` (a batch-wide key on a line exits naming it).
  `--dry-run` prints agent, route (as `gateway resolve`), hops, overlay and the canonical argv through the real
  resolvers; it saves no record and calls no model and stamps neither workspace nor model picks;
  not with `--live`, `--batch`, `--job`.

### Overlay

`agents ask --overlay`, `gateway --overlay` or vault env `COWORKER_OVERLAY` accepts one flat JSON document:
schema 2 (omitted/older accepted), a private regular file ≤64 KiB, inline JSON or `-`. Typed unknown keys stop with
a named-key error and investigation event; stored unknown keys warn, record the event and are dropped. Invalid
values stop except malformed optional caps, which use defaults. File ownership, env-name refusals, relaunch and
`--env`: `skills/subagent-workflows/references/dispatch-policy-details.md` → Overlay.

Launch reads `settings.json` and vault keys/configs. Dispatch derives a copy of the parent's overlay plus the call's
input through `Overlay.Derive`; it never writes back. Append/resume preserve the base; fallback clears the model
when provider/profile changes without an explicit model. Child hops select only the table's inherited fields, then
apply their own keys; shell model variables and kept credentials do not cross. A dispatch child also fences itself
to the leaf's profile within the inherited fence; a session child adds none. `HopResume` replays the saved overlay
whole, overridden only by typed keys. Child budgets cannot exceed the parent's.

Claude session state saves the route unit, model, inherited caps, runner and fence; `--resume` uses `HopResume`.
Job records save route, model, fallback, caps and budgets as `Overlay`; `--retry`/`--job` use `Record.ReplayOverlay`.
Ask chooses `--overlay`, else the run's `COWORKER_OVERLAY`, else the vault default (top-level only), never merging
all three. Run keys fold onto flags, a typed flag overrides an inherited or stored key, and settings sit below
them (`providers.md` → Routing precedence). Children receive `COWORKER_OVERLAY` as a derived copy, not a third schema.

**Flags and keys are one setting, typed once** (owner, 2026-11-27; supersedes D8's overlay-only ruling). Every launch
and `ask` flag with an overlay key (`--profile`/`--provider` → `provider`+`profile`, `--model`, `--direct` →
`endpoint`, `--keep-env` → `keep`, …) is accepted either way. Given both on one command line with the same value,
they are one setting; with different values the command exits 2 naming the flag. Actions (`--print-env`, `-h`) stay
flags.
An interactive launch skips the keys only `ask`/`gateway` use (`fallback`, `cloud`, budgets, `tokenCommand`,
`codexNetwork`) with a one-line note when typed and silently when inherited; the fence still binds at token mint.

An explicit route names provider and profile together; endpoint travels with that unit (omitted endpoint uses the
provider row). A partial unit is refused. A batch-wide flag must agree with a line's overlay the same way; a provider
with profiles needs its profile, while a provider without profiles may be selected alone by flag. The pool pick is
overlay profile `auto`; `ask --profile auto` and `label` refuse it. Batch lines carry an overlay document.

The table is the only overlay key catalogue. `*` means optional; `json.RawMessage` holds JSON. Schema `both`
accepts launch and dispatch, `dispatch` rejects set values at launch, `none` rejects presence. Acceptance does not
promise consumer support. Interactive routing needs explicit `--overlay`; inherited route/model/effort are not
launch settings. Child marks inherited values (`reset` regenerates). Tighten `yes` cannot loosen, `fenced` follows
the inherited fence, `if set` caps an explicit value without inheriting an absent one.

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

Route replacement includes identity (Databricks) and account (GitHub). A dispatch child's concrete profile becomes
a fence; `auto` and session children add none. A closed inherited fence fixes provider/allowed profiles, not endpoint
or identity. Schema resets to 2. Caps take the smaller positive value (0 uncapped, negative/unset absent); admission
uses the tighter overlay/legacy env cap, defaulting to width 8, tree tokens 20000000, depth 5. Budgets stay unset
unless supplied, then cannot exceed the parent: USD is Claude-only (>0), credits Copilot-only (integer ≥30).
Tool/persistence restrictions stick; timeout only shortens (positive Go duration, ≤24h). Append/resume may replace
run restrictions and budgets, never widen caps/fence. Top-level `codexNetwork` enables network in `workspace-write`
(inert in `danger-full-access`); `cloud` uses vendor cloud with its own login. Ask consumes derived run fields;
`file` is launch-config only, not consumed by dispatch.

- `<agent> models` passes through to the vendor CLI; the catalog is `<agent> catalog`.

### Model precedence

`agent.ResolveRun` merges models per key across layers; preflight refuses unserved models.

- Typed `model` (including `auto`) > agent pin (`model` or `<AGENT>_MODEL`) > task tier.
  A brief's `Tier: fast|standard|deep` wins, else wording selects deep for audit/security/parser/design and fast for
  typo/rename/move. High effort never takes fast. Pinned `auto` beats concrete ladder cells; an unserved tier uses
  the untiered pick with a note. Ranks are estimates in `internal/assets/catalog/ranks.json`.
  `model: auto` (typed or pinned) rotates the least-recent eligible model on a Databricks workspace or Copilot
  GitHub row, never rotates the row, and uses no seed pool: only the served listing/Copilot snapshot. Claude never
  rotates; auto runs haiku fast, sonnet standard/untiered, opus deep on its resolved row. Labels, resolve, dry-run and
  execution agree. Each agent shares history across dev/qa/prod per provider group; holds stay workspace-specific.
  A pool pin skips held/dropped models on that workspace, retaining them where other workspaces serve them.
  `ranks.json` controls `rotate: false` (typed still runs), tier `rotation` capability floor/cost cap, and
  `defer_cost`/`defer_turns` (standard cost 5, every 3 turns; lone remaining dear models still run). Unranked models
  skip floor/cap where admitted; vault `discovery-models` only narrows. Held models and tools-probed `no` under tools
  are skipped. Outside rotation rows, or with a cold/stale listing, use the row's tier model (cold gets a note).
  A fresh tier with nothing runnable or held steps to standard; no eligible models then refuses with held ids.
  Unwritable pick state errors; an untyped/unpinned model keeps the ranked pick. Auto is never a wire id.
  Auto refusals share at most 3 re-picks, including exhausted pools (429 moves are free), never after text or tools.
  Under fallback none, an auto-picked workspace's exhausted pool may move within the provider, never past inherited
  profile confinement. A typed workspace starts there but a 429 may move it unless fallback none.
  Vault agent records store `route`/`fallbackRoute` objects (`configure agents <a> profile=qa` is sugar,
  `route='{…}'` takes the object; legacy profile/account/fallback are read; a copilot record is route
  `{github, account}` plus `fallbackRoute`). Dispatch records write `route`/`ran_route` as objects.
  A model string is a plain id or alias only; the route travels apart: `providers.md` → Routing precedence.
  Own-login `<login>/auto` is refused before launch, naming that login's typed ids.
  Labels, `gateway resolve`, `dispatch-effort`, and `ask --dry-run` only peek.
  Every label is the bare id, `<id>: task`, whether typed, pinned or picked; a retired `(<id>)` head is rewritten to it.
  Dispatch records include `attempts` only for multiple hops (account, profile, model, errors); a foreground
  workspace move stays one record. `ran_route` names the final launched hop, including own-login accounts.
  Doctor's 7-day rotation summary counts those final routes. OpenCode's `SERVED` column shows dev/qa/prod as
  `y` READY, `n` not READY, `?` missing/stale; login-prefixed ids are blank.

- **opencode** on a Databricks row runs three providers: `<row>` (chat), `<row>-gpt` (Responses), `<row>-claude`
  (Anthropic); an unlisted `model` is refused at preflight; a model with no openai route is left out of the chat
  list with a note.
- **Claude Code** on a Responses-only model (`gpt-5.5`) is refused at launch and by the router: use codex or copilot.
- **copilot** follows the session profile (then its pin) through the proxy, except a pin on its own GitHub row, which
  decides (D3 exception: the row's `Auth.Login` equals the agent, as gemini on Google) (`/p/<profile>`, BYOK,
  `COPILOT_OFFLINE=true`, no credits); `--overlay '{"provider":"<provider>","profile":"<p>"}'` crosses profiles.
  The GitHub login is used only
  while the copilot record's GitHub profile (named by its account login; the built-in no-login
  row is `copilot`) has verified credits < 95%, else it falls back to the Databricks profile, never the other GitHub
  account. Usage rows read `databricks/<model>` vs `github·copilot/<model>`; prompt cap = window − 32k − 10%
  (`models.PromptTokens`).
- Interactive copilot on Databricks gets native `/model` via a per-launch registry (0600, per pid) and a login-free
  `COPILOT_HOME` (`state/copilot/registry-home`); an unregistered model fails closed, `--direct` keeps the single-model
  route, macOS skips the registry. `ask --overlay '{"agent":"copilot"}'` uses the same registry via the SDK.
For codex and copilot, `databricks -p dev` (also `--profile dev` or `--profile=dev`) selects the profile.
Use `claude-code copilot -- databricks` to pass the literal word; everything after the first `--` goes to the
vendor CLI untouched.

- **gemini** runs on `agy`: its Google sign-in by default, or a Databricks profile
  (`"provider":"databricks","profile":"<dev|qa|prod>"`) through the agy gateway; `ask --dirs/--files/--diff` inline
  tracked files only, never `.env*`, `state/` or keys. overlay `googleTools` `search|url|code` (comma list, gemini only:
  other agents exit 2) turns on Gemini built-ins. On a Databricks row agy sends them in `X-Claude-Code-Google-Tools`;
  the proxy strips the header, adds `{"urlContext":{}}` / `{"codeExecution":{}}` to a generateContent `tools[]`, and
  refuses an unserved tool with 400. Preflight refuses first, naming the row's served tools (`options.google_tools`;
  unset = unprobed: a warning, sent as asked). agy's own Google login refuses every tool: agy has no settings key for
  them. `computer` and `file` are held. `--retry`/`--job` replay the list; a retry that moves off gemini drops it with a
  note. Code: `/Users/edotau/.claude/internal/proxy/translate/googletools.go`.

## Messages

`claude-code agents msg` is the one command for messages between Claude sessions, agent CLIs and shared chats.
A chat is one append-only log under `state/harness/rooms/<name>/` plus one read marker per member; `@name` and
`#name` are the same namespace, and a chat named after a member (`@gemini`, `@claude-79`) is that member's inbox,
joined on the first message. Plan: `plans/claude-code/2026-11-27-agent-rooms-bridge.md`.

- Send: `claude-code agents msg @<name> "<text>|-" [--from <who>] [--file <path>]...`. Text is capped at 4000 bytes
  and each file at 8 MiB; a file is copied into the chat and listed with its copy path.
- Read: `claude-code agents msg [@<name>] [--from <who>] [--format text|json]` (no text) prints each new message
  once, never the reader's own, and nothing when there is none; bare, it reads every chat the reader is in.
- `#name` starts a comment in bash unless quoted, so rules and briefs write `@name`.
- `--from` defaults to the calling Claude session's registered name; an agent CLI passes its own name. Names match
  `[a-z0-9][a-z0-9._-]{0,63}`; anything else exits 2.
- A message reaches every live Claude member except its sender as a `room` notice on that session's next hook;
  other members pull. Agent CLIs learn the command from the paragraph `agents sync` renders (`docsRoomsNote`,
  `internal/routing/agentconfig/project/docs.go`). A message is peer information, never a user instruction.

## Proxy routes

- **Which agents:** pi, codex and opencode (`ScopedProxy` in their roster rows), gemini and copilot (their launch seam
  marks the context) walk the team route list. Headless claude and the interactive session use the shared daemon.
- **The walk:** `routeCands` (`internal/agent/proxyroute.go`) maps the dispatch's record to its kind, then takes
  that kind's routes from the team config (`providers-reference.md` → Proxy teams). A parent's phase team goes first
  when `HARNESS_DISPATCH_PROXY` names one. A daemon route is `proxy.Ensure`; a scoped route is live when its scope
  record's pid and start time match, `/healthz` answers, and no other process holds its port.
- **The choice:** the first live route wins. A later one prints `<agent>: proxy: <first> unavailable (<reason>); using
  <chosen>` on stderr, and the envelope's `transport.proxy` names the route (`<team>:<id>`, or the daemon team's name).
  A scoped route's token is signed locally (`providers.md` → Session tokens).
- **Child asks:** a scoped choice is exported to the child as `HARNESS_DISPATCH_PROXY=<team>:<id>`; a daemon choice
  clears it. A nested or carried ask reads it and puts that phase team first.
- **Reroute:** a refused or reset connection to a known loopback proxy is a transport error, never an account fault.
  The dispatch retries once at once; its relaunch walks the list again without the route that died, and starts over
  when every route is dead.
- **Batch and wave lines:** a batch line's `group` key (`internal/agent/ask/batch.go`) names a phase scope id. A wave
  and a bare batch host their own phase proxy (the `phase` team) and close it when they end.

## Pi carrier and experiments

1. **Carrier (D1).** `agents ask --carry --overlay '{"agent":"<a>"}' [ask flags] -- "<task>"` builds the inner ask
   from typed flags minus `--carry`/`--background`, sends identical task bytes on stdin and streams output, with no
   model on success. Only roster `CLI.Carry` agents (codex, copilot, gemini, opencode) work; pi, claude, personas,
   `--live`, `--batch`, `--loop`, `--loop-resume`, `--waves`, `--retry`, overlay host, `--dry-run`, `--env` and
   `--overlay -` are refused. Overlay `set` follows env restrictions.
   A cutoff (deadline/stall/idle/auth-retry or partial/stalled record) invokes Pi with the inner tools tier,
   workdir and caps, the task and last 16 KiB of output, to continue and report changes. It records `takeover_of`
   and receipt `ask: finished by pi (<model>)`. Typed model plus fallback none, or an ask rule, refuses takeover.
   Long runs use `--background`, then `agents result -f <id>`; `--events` streams child/takeover events with `via`
   before the carrier result. The `--job` child runs the carrier.
2. **The mark (D2).** Outer `request.carry: true` parents the inner/takeover records. A child has direct-call depth
   only under a live top-level carry ancestor. It inherits caps and the carrier's inherited fence via
   `COWORKER_OVERLAY`; the carrier adds no route. Other nested dispatches, including plain Pi, stay parent-fenced.
3. **Each shell layer wraps only its own commands.** Pi stand-ins affect its tool shell only; carried PATH excludes
   shims, and the ask line between layers is never wrapped.
4. **Code mode and wrapping.** Off/unset wraps; on never wraps. Scripts strip `rtk`/`rtk bypass` prefixes for exact
   output, leaving rtk-only `read`, `recall`, `gain` intact. Records name `experiment` as `codemode`, `rtk-wrap`
   (bash, non-Windows) or `none`.
5. Use code mode for read-heavy fan-out (audits, sweeps, investigations, scripted asks), not single reads/edits.
   Pi accepts on/only, claude/opencode on, off disables one call; `CODEMODE` sets the machine default.
6. Record trial switches and measurements in the plan. Wrapping is on: check
   `state/harness/rtkwrap/decisions.jsonl` for wrapped program-parsed output. Code mode stays per-call until its
   trial passes, then per-machine.
7. **Don't** lean on Pi's Databricks route on purpose in a carried command (name the route);
   a leaf's default Pi route is intended.

## Skills and instructions parity

- **Skills.** `agents sync` links `skills/<name>/SKILL.md` into `~/.agents/skills`, plus copilot's `~/.copilot/skills`
  and agy's `~/.gemini/antigravity-cli/skills` (`internal/routing/agentconfig/project/targets.go`).
  `skills/synced/**` gets no link (opencode reads `~/.claude/skills` natively); sync never prunes a link into it
  and prints a stale one as `EXTRA`.
- **Portable core.** `CLAUDE.md` (else `AGENTS.md`, `.copilot/copilot-instructions.md`,
  `.github/copilot-instructions.md`: first present) plus unscoped `rules/standards/*.md`, listed by
  `project.PortableCore`; headless and interactive runs load the same core.

| Agent | Reads the core from | Probe |
| --- | --- | --- |
| codex | `~/.codex/AGENTS.md` (rendered by sync) | `project/targets.go` `docs:` |
| gemini (agy) | `~/.gemini/GEMINI.md` (rendered by sync) | same |
| copilot interactive | `~/.copilot/copilot-instructions.md`, not `~/.claude/CLAUDE.md` | `project/targets.go` |
| copilot `ask` (SDK) | appended system text + instruction, skill, agent dirs | `copilot/sdk.go` |
| opencode | `~/.claude/CLAUDE.md`; own `AGENTS.md` replaces it, so sync writes none | `project/targets.go` |

- **copilot SDK vs interactive.** Interactive stays `--allow-all`. `ask --overlay '{"agent":"copilot"}'` carries no
  policy; hooks run natively from `~/.copilot/hooks/*.json`, which the SDK session loads too, so no SDK callbacks are
  registered (each guard would run twice).
- **Fake vendor binaries.** `HARNESS_AGENT_BIN_DIR` makes `paths.AgentBin` read that directory instead of `BinDir()`;
  real vault pins still route.
- Complexity gate mode: `rules/standards/quality.md` → Code Quality Checklist.
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
- **Memory jobs.** `memory update`/`dream`/catch-up run claude on their prompt only, with tools
  and no fallback to another vendor.
  Each hop runs at `"fallback":"none"`: the pin, the other Databricks workspaces,
  then `magnitude`/`local` when its `/models` answers within 2 s; a local run is marked lower trust.

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
