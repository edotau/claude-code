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
  Codex/Copilot hooks). Keys are referenced by env var and exported only by a routed launch.
- opencode's `~/.config/opencode/` gets only the theme file and `agents sync`'s agent files; model/MCP/theme config
  come from a tracked overlay injected at launch (`OPENCODE_CONFIG` / `OPENCODE_TUI_CONFIG`,
  `internal/agentconfig/template/`).
- Each agent-config overwrite keeps 5 timestamped backups; `configure --revert [--agent a]` steps back one version.
- Codex pins live in the vault codex agent record (legacy `codex-settings` overlay read until migrated) and apply
  to `$CODEX_HOME/config.toml`; its snapshots and baseline are vault-encrypted (`settings_backups`, rotated with
  the key, excluded from exports), last 5 kept. Provider config, hook trust and unknown native keys are preserved;
  an unavailable vault key aborts changed writes.
- Hook commands rendered into `~/.codex` carry `HARNESS_HOOK_AGENT=codex`; any `HARNESS_HOOK_*` prefix counts as
  harness-owned on re-render.

## Dispatch behavior

- `ask --format json` envelope: `agent`, `status` (`ok|error|partial|quota|stalled`), `model`, `answer`, `usage`,
  `duration_ms`, `fallback*`, `handoff`, `transport`. Isolation flags: `--no-persist` (claude, codex), `--safe-mode`
  (claude, codex, opencode), `--debug-file <path>` (claude, gemini). Without `--tools`, agents are read-only.
  `--read-only` (implies `--tools`): copilot approves file reads, URL fetches and read-only git; gemini denies
  `Bash`, `Write` and `Edit` through the agy `permission-gate` hook (reads, search and URL fetches still run; refused
  when the hook is missing); opencode enforces those denies with read tools on, refusing `--safe-mode`;
  any other agent drops its tools (fail closed).
  copilot `--tools` approves every request like the other agents' `--tools`, still subject to the dispatch's
  permission rules (a Deny returns feedback and the run continues) and the native hooks.
  claude `--tools` off Anthropic is an allow list (edit tools plus go/git/read commands, `--allow` adds to it), not
  approve-all: there is no judge there, so anything unlisted is denied.
- `agents ask --agents <json|file>` defines agents inline: `name`, `description`, `prompt`, `tools`, `model`,
  `max_turns`, `agent`, `profile`, `effort`. `--agent` also takes an `--agents` key, and a definition's `model` applies
  on every agent. With `--batch`, `--agents` covers the whole file and a line's `agent` takes the same selector; the
  retired per-line key is an error. `--host` with `--agents` exits 2. Go API: `agent.Client`, `Client.Stream`.
- `agents ask --overlay <value>` / `gateway --overlay <value>` / `COWORKER_OVERLAY` (read when the flag is absent; the
  process env first, then the vault env for a dispatch that is not nested; interactive launches never read the vault's):
  ONE keyed JSON document (`schema` 1, all keys optional; `gateway --overlay` previews only the confinement plus `model`
  and `fallback`, ignoring `effort`, `host`, `remote` and the env layer): `provider` (`anthropic`, `openai`, `github`,
  `google`, `databricks`; omitted = the agent's pin; an agent that cannot use it is refused: claude anthropic, codex
  openai, copilot github, gemini google, opencode databricks only, every agent databricks; `databricks` confines only
  the Databricks hop, so unless `fallback` is `none` the dispatch may still fall back to the agent's own login, then
  `claude`), `profile` (one under it; databricks: or `auto`), `model`, `effort`, `fallback`, `host`, `remote` (like
  global `--remote`, `auto` allowed; refused with a host) and the env layer `set`/`unset`/`keep`. Value: a file path
  (regular, yours, not group/world-writable, <= 64 KiB), inline `'{...}'` or `-` (stdin; the task is then an argument);
  an unknown key or bad value exits 2. Typed flags win, except a typed `--profile`/`--provider`/`--model <p>:<alias>`
  outside the overlay's provider/profile is refused (exit 2). Precedence: typed flag > `--overlay`, else
  `COWORKER_OVERLAY` > `--retry`/`--job` record > process env > vault env > agent definition > agent pin > table
  default. Nested dispatches inherit only the confinement (`COWORKER_OVERLAY` cut to `provider`/`profile`; a standing
  default is the vault env `COWORKER_OVERLAY`); the rest of the document is one hop, and
  `--job`/`--background`/`--batch` relaunches carry it. `--host` carries the confinement (`provider`/`profile`) to the
  far machine and refuses a non-empty `set`/`unset`/`keep`; `--live` refuses the env layer too. `--retry` replays the
  recorded workspace unless the overlay names a provider. Interactive launches read it for `set`/`unset`/`keep` and
  refuse routing keys. Env layer: `files` reserved, refused; `set` values may be `@secret:<NAME>` (harness-internal
  secrets refused); a `set` name the launch already sets for its route is refused (`--env` still overrides); endpoint,
  proxy and TLS names (`*_BASE_URL`, `*_API_BASE`, `*_ENDPOINT`, `*_GATEWAY_URL`, `*_PROXY_URL`, `*_CA_CERT`,
  `DATABRICKS_HOST`, `HTTP(S)_PROXY`, `ALL_PROXY`, TLS overrides, `NODE_OPTIONS`) are refused in `set`, `keep` and
  `unset`, as are agent-config locations (`OPENCODE_CONFIG`, `CODEX_HOME`, `COPILOT_HOME`, `HOME`, `GH_CONFIG_DIR`, …):
  use `--profile`; `keep` refuses credential and `HARNESS_*` names, `unset` `HARNESS_*` names (`HARNESS_KEEP_ENV`
  stays the escape hatch). `--env` / `--keep-env` are its one-line shorthand (the same endpoint/proxy/TLS refusals;
  credential names in `--keep-env`, ask and interactive).
- `<agent> models` is not a harness subcommand: it passes through to the vendor CLI. The catalog is `<agent> catalog`.
- **opencode** on a Databricks row runs three providers: `<row>` (chat), `<row>-gpt` (Responses), `<row>-claude`
  (Anthropic); a `--model` the workspace lists no endpoint for is refused at preflight. An `ai-gateway` row has no
  openai route, so its chat models are left out with a note.
- **Claude Code** on a Responses-only model (`gpt-5.5`) is refused at launch and by the router — use codex or copilot.
- **copilot** follows the session profile through the proxy (`/p/<profile>`, BYOK, `COPILOT_OFFLINE=true`, no
  credits); `--model <profile>:<alias>` crosses profiles. The GitHub login is used only while the copilot record's
  GitHub profile's verified credits are < 95%, else it falls back to the Databricks profile (never the other GitHub
  account).
  Usage rows read `databricks/<model>` vs `github·copilot/<model>`. Prompt cap = window − 32k − 10%
  (`models.PromptTokens`).
- Interactive copilot on Databricks gets native `/model` via a per-launch registry (0600, per pid) and a login-free
  `COPILOT_HOME` (`state/copilot/registry-home`); the proxy takes a session token, so no secret sits on disk. An
  unregistered model fails closed; `--direct` keeps the single-model route; macOS skips the registry (Keychain
  GitHub login still readable).
  `ask --agent copilot` uses the same registry via the SDK.
- **copilot** and **opencode** interactive launches reuse the repo's last-used profile before the agent record's
  profile and treat it like the record (copilot's credit gate applies); `<agent> profile` or `configure` record
  changes clear it.
- **gemini** runs on `agy`: its Google sign-in by default, a named Databricks profile (`--profile dev|qa|prod`) through
  the agy gateway; `ask --dirs/--files/--diff` inline tracked files only, never `.env*`, `state/` or keys.

## Skills and instructions parity

- **Skills.** `agents sync` links `skills/<name>/SKILL.md` (symlinked dirs resolving inside `skills/` included) into
  `~/.agents/skills`, plus copilot's `~/.copilot/skills` and agy's `~/.gemini/antigravity-cli/skills`, which do not
  read the shared dir (`internal/agentconfig/project/targets.go`). **`skills/synced/**` (claude.ai account skills)
  gets no link**, but opencode reads `~/.claude/skills` natively and loads it (duplicate names load once);
  `discover.go` skips it and `skills.go` never prunes a link into it — a link made before the
  exclusion (e.g. `~/.copilot/skills/synced`) stays visible to that agent; sync prints it as `EXTRA`.
- **Portable core.** `CLAUDE.md` (else `AGENTS.md`, `.copilot/copilot-instructions.md`,
  `.github/copilot-instructions.md`: first present)
  plus unscoped `rules/standards/*.md` (no `paths:` frontmatter), listed by
  `project.PortableCore`; headless and interactive runs load the same core. Where each agent reads it:

| Agent | Reads the core from | Probe |
| --- | --- | --- |
| codex | `~/.codex/AGENTS.md` (rendered by sync) | `project/targets.go` `docs:` |
| gemini (agy) | `~/.gemini/GEMINI.md` (rendered by sync) | same |
| copilot (interactive) | `~/.copilot/copilot-instructions.md` (rendered), not `~/.claude/CLAUDE.md` | `project/targets.go` (copilot `docs:`) |
| copilot `ask` (SDK) | appended system text + instruction, skill, agent dirs | `copilot/sdk.go` |
| opencode | `~/.claude/CLAUDE.md` natively; its own `AGENTS.md` **replaces** it, so sync writes none | `project/targets.go` (opencode: no `docs:`) |

- **copilot SDK vs interactive.** Interactive stays `--allow-all`. `ask --agent copilot` runs through the SDK:
  per-call `Decide` on the dispatch's permissions; hooks run natively from `~/.copilot/hooks/*.json`, which the SDK
  session loads too (live, 1.0.105), so no SDK callbacks are registered (they would run each guard twice).
- **Fake vendor binaries.** `HARNESS_AGENT_BIN_DIR` makes `paths.AgentBin` read that directory instead of `BinDir()`
  (no fallback to the real install); real vault pins still route.
- **Deny and D3.** Deny is the hard boundary; Ask goes to the agent's own judge (opencode Ask = `--auto`). A Deny an
  agent cannot enforce refuses the dispatch with `retry.Error{Kind: retry.KindFatal}` naming the rule. opencode
  `--read-only` keeps read tools on, denies `Bash`, `Write` and `Edit`, and runs without `--auto`.
- **Bash deny via `permission-gate`.** opencode's native matcher is bypassed by `bash -c "..."`, so the dispatch's
  rules ride `HARNESS_DISPATCH_PERMISSIONS` to the `permission-gate` PreToolUse module (`Decide` per segment): opencode
  runs it from the harness plugin's `tool.execute.before` (headless and TUI; refused only under `--safe-mode`, which
  loads no plugin), codex and agy as a native PreToolUse hook. Hook modes: `hooks.md` → Applicability. A live codex or
  agy home picks up the hook only after the owner runs `configure --restore --agents --agent codex` (or `gemini`),
  which re-renders only that agent's config; until then those agents keep refusing a Bash deny.
- **Memory jobs (claude, gated).** `memory update`/`dream`/catch-up run claude with `RunOptions.Gate`:
  `bypassPermissions` (off Anthropic, `dontAsk` hard-denies writes under `.claude/`), `--strict-mcp-config`, tools
  limited to `Read,Glob,Grep,Edit,Write,MultiEdit,Bash`, and the `permission-gate` hook merged into the launch's one
  `--settings` (`exit 2` on any failure). Policy: plan mode, Edit/Write allowed under the job's bank dirs (dream: the
  memory root and `CLAUDE.md`), Bash only dream's `claude-code memory timeline|review|index|promote`; no policy, binary
  or policy env refuses the launch.

## Codex child routing

- Native `spawn_agent` children use the Codex routing cells on a Databricks profile with a validated catalog — only
  explicitly fresh children (`fork_turns="none"` or `fork_context=false`) with no model/effort pin or custom type.
  The child keeps its provider; the model must support Responses and be in the session catalog.
- A codex record `model` or `effort` disables child routing for the whole session (clear the effort in
  `claude-code configure codex`).
- Codex-only hooks log starts, decisions, spawn results and child models to
  `state/harness/codex/routing-<session-hash>.jsonl` (0600, no prompts). A decision is verified only when the spawn
  result's agent ID joins a child-start event; otherwise `matches_router_decision` stays `null`.
- `doctor` inspects Codex machine and `CODEX_HOME` config by metadata only: policy presence leaves the route unverified.
