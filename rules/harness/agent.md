---
paths:
  - "internal/agent/**"
  - "internal/copilotsdk/**"
  - "internal/launch/**"
---
# Agent CLIs — codex, copilot, opencode, gemini

## Config writes

- Agent CLI configs are merged, never replaced (`~/.codex/config.toml`, `~/.gemini/antigravity-cli/settings.json`,
  Codex/Copilot hooks). Keys are referenced by env var and exported only by a routed launch.
- opencode's `~/.config/opencode/` gets only the theme file and `agents sync`'s agent files; model/MCP/theme config
  come from a tracked overlay injected at launch (`OPENCODE_CONFIG` / `OPENCODE_TUI_CONFIG`,
  `internal/agent/template/`).
- On a terminal, `configure --restore` prompts before overwriting hand-edited lines; non-interactive runs write and
  warn. Each overwrite keeps 5 timestamped backups; `configure --revert [--agent a]` steps back one version.
- Codex pins live in the vault (`codex-settings` overlay) and apply to `$CODEX_HOME/config.toml`; its snapshots
  and baseline are vault-encrypted (`settings_backups`, rotated with the key, excluded from exports), last 5 kept.
  Provider config, hook trust and unknown native keys are preserved; an unavailable vault key aborts changed writes.
- Hook commands rendered into `~/.codex` carry `HARNESS_HOOK_AGENT=codex`; any `HARNESS_HOOK_*` prefix counts as
  harness-owned on re-render.

## Dispatch behavior

- `ask --format json` envelope: `agent`, `status` (`ok|error|partial|quota|stalled`), `model`, `answer`, `usage`,
  `duration_ms`, `fallback*`, `handoff`, `transport`. Isolation flags: `--no-persist` (claude, codex), `--safe-mode`
  (claude, codex, opencode), `--debug-file <path>` (claude, gemini). Without `--tools`, agents are read-only.
  `--read-only` (implies `--tools`): copilot approves only file reads and URL fetches; any other agent drops its
  tools (fail closed).
- `ask --agents <json|file>` defines agents inline: `name`, `description`, `prompt`, `tools`, `model`, `max_turns`,
  `runner`, `profile`, `effort`. `--agent` also takes an `--agents` key, and a definition's `model` applies on every
  agent. With `--batch`, `--agents` covers the whole file and a line's `agent` takes the same selector; the
  retired per-line key is an error. `--host` with `--agents` exits 2. Go API: `agent.Client`, `Client.Stream`.
- The agent subcommand `models` is a compatibility alias for `catalog`; top-level `claude-code models` is separate.
- **opencode** on a Databricks row runs three providers: `<row>` (chat), `<row>-gpt` (Responses), `<row>-claude`
  (Anthropic); an off-menu `--model` is refused. An `ai-gateway` row has no openai route, so its chat models are
  left out with a note.
- **Claude Code** on a Responses-only model (`gpt-5.5`) is refused at launch and by the router — use codex or copilot.
- **copilot** follows the session profile through the proxy (`/p/<profile>`, BYOK, `COPILOT_OFFLINE=true`, no
  credits); `--model <profile>:<alias>` crosses profiles. The GitHub login is used only while the `COPILOT_PROFILE`
  pin's verified credits are < 95%, else it falls back to the Databricks profile (never the other GitHub account).
  Usage rows read `databricks/<model>` vs `github·copilot/<model>`. Prompt cap = window − 32k − 10%.
- Interactive copilot on Databricks gets native `/model` via a per-launch registry (0600, per pid) and a login-free
  `COPILOT_HOME` (`state/copilot/registry-home`); the proxy takes a session token, so no secret sits on disk. An
  unregistered model fails closed; `--direct` keeps the single-model route; macOS is refused until the Keychain check
  lands. `ask --agent copilot` uses the same registry via the SDK.
- **copilot** and **opencode** interactive launches reuse the repo's last-used profile before the `*_PROFILE` pin and
  treat it like the pin (copilot's credit gate applies); `<agent> profile` or `configure` pin changes clear it.
- **gemini** runs only on `agy` and its Google sign-in; `gemini bridge` inlines tracked files only, never `.env*`,
  `state/` or keys.

## Codex child routing

- Native `spawn_agent` children use the Codex routing cells on a Databricks profile with a validated catalog — only
  explicitly fresh children (`fork_turns="none"` or `fork_context=false`) with no model/effort pin or custom type.
  The child keeps its provider; the model must support Responses and be in the session catalog.
- A vault `CODEX_MODEL` or `CODEX_EFFORT` pin disables child routing for the whole session (clear it in `vault edit`).
- Codex-only hooks log starts, decisions, spawn results and child models to
  `state/harness/codex/routing-<session-hash>.jsonl` (0600, no prompts). A decision is verified only when the spawn
  result's agent ID joins a child-start event; otherwise `matches_router_decision` stays `null`.
- `doctor` inspects Codex machine and `CODEX_HOME` config by metadata only: policy presence leaves the route unverified.
