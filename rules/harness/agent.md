---
paths:
  - "internal/agent/**"
  - "internal/agentconfig/**"
  - "internal/copilot/**"
  - "internal/launch/**"
---
# Agent CLIs — codex, copilot, opencode, gemini

## Config writes

- Agent CLI configs are merged, never replaced (`~/.codex/config.toml`, `~/.gemini/antigravity-cli/settings.json`,
  Codex/Copilot hooks). Keys are referenced by env var and exported only by a routed launch.
- opencode's `~/.config/opencode/` gets only the theme file and `agents sync`'s agent files; model/MCP/theme config
  come from a tracked overlay injected at launch (`OPENCODE_CONFIG` / `OPENCODE_TUI_CONFIG`,
  `internal/agentconfig/template/`).
- Each agent-config overwrite keeps 5 timestamped backups; `configure --revert [--agent a]` steps back one version.
- Codex pins live in the vault (`codex-settings` overlay) and apply to `$CODEX_HOME/config.toml`; its snapshots
  and baseline are vault-encrypted (`settings_backups`, rotated with the key, excluded from exports), last 5 kept.
  Provider config, hook trust and unknown native keys are preserved; an unavailable vault key aborts changed writes.
- Hook commands rendered into `~/.codex` carry `HARNESS_HOOK_AGENT=codex`; any `HARNESS_HOOK_*` prefix counts as
  harness-owned on re-render.

## Dispatch behavior

- `ask --format json` envelope: `agent`, `status` (`ok|error|partial|quota|stalled`), `model`, `answer`, `usage`,
  `duration_ms`, `fallback*`, `handoff`, `transport`. Isolation flags: `--no-persist` (claude, codex), `--safe-mode`
  (claude, codex, opencode), `--debug-file <path>` (claude, gemini). Without `--tools`, agents are read-only.
  `--read-only` (implies `--tools`): copilot approves file reads, URL fetches and read-only git; gemini refuses
  (agy has no read-only mode: use `--tools` or another agent); any other agent drops its tools (fail closed).
  copilot `--tools` is scoped (reads, URL fetches, writes inside `--workdir` outside `.git`/`.env*`/`state/`/keys, an
  allowlisted shell from `internal/agentconfig/agents.json`, indexed in `internal/agentconfig/permissions.go`);
  refused calls return feedback and the run continues. `--tools --autonomous` approves every request.
- `agents ask --agents <json|file>` defines agents inline: `name`, `description`, `prompt`, `tools`, `model`,
  `max_turns`, `agent`, `profile`, `effort`. `--agent` also takes an `--agents` key, and a definition's `model` applies
  on every agent. With `--batch`, `--agents` covers the whole file and a line's `agent` takes the same selector; the
  retired per-line key is an error. `--host` with `--agents` exits 2. Go API: `agent.Client`, `Client.Stream`.
- `ask --overlay <file>` / `COWORKER_OVERLAY`: per-launch `{set,unset,keep}` over the child's env (`files` reserved,
  refused); `set` values may be `@secret:<NAME>` (harness-internal secrets refused); a `set` name the launch already
  sets for its route is refused (`--env` still overrides); endpoint, proxy and TLS names (`*_BASE_URL`, `*_API_BASE`,
  `*_ENDPOINT`, `*_GATEWAY_URL`, `*_PROXY_URL`, `*_CA_CERT`, `DATABRICKS_HOST`, `HTTP(S)_PROXY`, `ALL_PROXY`, TLS
  overrides) are refused in `set` and `keep`: use `--profile`; `keep` refuses credential names (`HARNESS_KEEP_ENV` stays
  the escape hatch). One hop: nested dispatches do not inherit it, `--job`/`--background`/`--batch` relaunches carry it,
  `--live` and `--host` refuse it. `--env` / `--keep-env` are its one-line shorthand (the same endpoint/proxy/TLS
  refusals; credential names in `--keep-env`, ask and interactive).
- The agent subcommand `models` is a compatibility alias for `catalog`; top-level `claude-code models` is separate.
- **opencode** on a Databricks row runs three providers: `<row>` (chat), `<row>-gpt` (Responses), `<row>-claude`
  (Anthropic); an off-menu `--model` is refused. An `ai-gateway` row has no openai route, so its chat models are
  left out with a note.
- **Claude Code** on a Responses-only model (`gpt-5.5`) is refused at launch and by the router — use codex or copilot.
- **copilot** follows the session profile through the proxy (`/p/<profile>`, BYOK, `COPILOT_OFFLINE=true`, no
  credits); `--model <profile>:<alias>` crosses profiles. The GitHub login is used only while the copilot record's
  GitHub profile's verified credits are < 95%, else it falls back to the Databricks profile (never the other GitHub
  account).
  Usage rows read `databricks/<model>` vs `github·copilot/<model>`. Prompt cap = window − 32k − 10%
  (`models.PromptTokens`).
- Interactive copilot on Databricks gets native `/model` via a per-launch registry (0600, per pid) and a login-free
  `COPILOT_HOME` (`state/copilot/registry-home`); the proxy takes a session token, so no secret sits on disk. An
  unregistered model fails closed; `--direct` keeps the single-model route; macOS is refused until the Keychain check
  lands. `ask --agent copilot` uses the same registry via the SDK.
- **copilot** and **opencode** interactive launches reuse the repo's last-used profile before the agent record's
  profile and treat it like the record (copilot's credit gate applies); `<agent> profile` or `configure` record
  changes clear it.
- **gemini** runs on `agy`: its Google sign-in by default, a named Databricks profile (`--profile dev|qa|prod`) through
  the agy gateway; `ask --dirs/--files/--diff` inline tracked files only, never `.env*`, `state/` or keys.

## Skills and instructions parity

- **Skills.** `agents sync` links `skills/<name>/SKILL.md` (symlinked dirs resolving inside `skills/` included) into
  `~/.agents/skills`, plus copilot's `~/.copilot/skills` and agy's `~/.gemini/antigravity-cli/skills`, which do not
  read the shared dir (`internal/agentconfig/project/targets.go`). **`skills/synced/**` (claude.ai account skills)
  goes to no agent**; `discover.go` skips it and `skills.go` never prunes a link into it — a link made before the
  exclusion (e.g. `~/.copilot/skills/synced`) stays visible to that agent; sync prints it as `EXTRA`.
- **Portable core.** `CLAUDE.md` plus unscoped `rules/standards/*.md` (no `paths:` frontmatter), listed by
  `project.PortableCore`; headless and interactive runs load the same core. Where each agent reads it:

| Agent | Reads the core from | Probe |
| --- | --- | --- |
| codex | `~/.codex/AGENTS.md` (rendered by sync) | `project/docs.go` `instructionFiles` |
| gemini (agy) | `~/.gemini/GEMINI.md` (rendered by sync) | same |
| copilot (interactive) | `~/.copilot/copilot-instructions.md` (rendered), not `~/.claude/CLAUDE.md` | F0 `p7` |
| copilot `ask` (SDK) | appended system text + instruction, skill, agent dirs | `copilot/sdk.go` |
| opencode | `~/.claude/CLAUDE.md` natively; its own `AGENTS.md` **replaces** it, so sync writes none | F0 `p2`, `p2b` |

- **copilot SDK vs interactive.** Interactive stays `--allow-all`. `ask --agent copilot` runs through the SDK:
  per-call `Decide` on the dispatch's permissions; hooks run natively from `~/.copilot/hooks/*.json`, which the SDK
  session loads too (live, 1.0.105), so no SDK callbacks are registered (they would run each guard twice).
- **Deny and D3.** Deny is the hard boundary; Ask goes to the agent's own judge (opencode Ask = `--auto`). A Deny an
  agent cannot enforce refuses the dispatch with `agent.Error{Kind: KindFatal}` naming the rule. opencode
  `--read-only` stays tools-off.
- **Bash deny via `permission-gate`.** opencode's native matcher is bypassed by `bash -c "..."`, so the dispatch's
  rules ride `HARNESS_DISPATCH_PERMISSIONS` to the `permission-gate` PreToolUse module (`Decide` per segment): opencode
  runs it from the harness plugin's `tool.execute.before` (headless and TUI; refused only under `--safe-mode`, which
  loads no plugin), codex and agy as a native PreToolUse hook. Hook modes: `hooks.md` → Applicability. A live codex or
  agy home picks up the hook only after the owner runs `configure --restore --agents --agent codex` (or `gemini`),
  which re-renders only that agent's config; until then those agents keep refusing a Bash deny.

## Codex child routing

- Native `spawn_agent` children use the Codex routing cells on a Databricks profile with a validated catalog — only
  explicitly fresh children (`fork_turns="none"` or `fork_context=false`) with no model/effort pin or custom type.
  The child keeps its provider; the model must support Responses and be in the session catalog.
- A codex record `model` or `effort` disables child routing for the whole session (clear it in `vault edit` →
  `agents.records.codex`).
- Codex-only hooks log starts, decisions, spawn results and child models to
  `state/harness/codex/routing-<session-hash>.jsonl` (0600, no prompts). A decision is verified only when the spawn
  result's agent ID joins a child-start event; otherwise `matches_router_decision` stays `null`.
- `doctor` inspects Codex machine and `CODEX_HOME` config by metadata only: policy presence leaves the route unverified.
