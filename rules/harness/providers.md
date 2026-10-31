---
paths:
  - "internal/providers/**"
  - "internal/vault/**"
  - "internal/models/**"
  - "internal/proxy/**"
  - "internal/router/**"
  - "internal/agent/route_*.go"
  - "internal/secrets/**"
---
# Providers, Models, Credentials

| To… | Run |
| --- | --- |
| Check binaries, providers, credentials, proxy | `claude-code doctor` · leaked credentials: `doctor --scan\|--git\|--secrets` |
| See providers and credential state | `claude-code configure provider --list [<name>]` |
| Set a credential | `claude-code vault set [NAME]` (bare on a terminal = guided dialog), or a provider's `auth.command` |
| Inspect or edit the vault | `claude-code vault list\|get\|delete\|import\|export\|edit\|rotate-secret` |
| Switch the session profile | `claude-code switch <profile> [--dry-run]` (relaunch) · one launch: `claude-code --profile <p>` |
| Pin models | `claude-code models --pin opus=<id>` · `--pin all=<id>` · `--unpin` (`--dry-run` previews) |
| List what a provider serves | `claude-code models --live [--profile <p>]` |
| Run the model proxy | `claude-code gateway proxy --start\|--stop\|--status` · foreground: `--serve --port N` |

- Secrets only in the vault or `auth.command` (CLAUDE.md hard rule 2); `configure provider --list` shows the
  source, never the value.
- Pins are vault-only (`CLAUDE_<TIER>_MODEL`); `--model` beats them. A `switch` pin lands in the vault too.
- Built-in providers are `internal/providers/defaults.json`; the vault's provider overlay replaces one by name.

## Model selection

Name the alias (`opus`/`sonnet`/`haiku`/`fable`), never a concrete model id: `internal/providers/defaults.json` plus
the vault overlay map each alias per provider.

- **`opus`** — architectural decisions, maximum-reasoning research and analysis.
- **`sonnet`** — planning, review, orchestration, exploration; the default `code-worker` leaf.
- **`haiku`** — high-frequency, cost-sensitive leaves and quick lookups.
- **Gemini** (`claude-code gemini bridge`) — sweeps whose context exceeds a session window (1M tokens).

## Served-aware Databricks routing

`configure routing` → `servedAware` (off by default) checks codex, copilot and opencode picks against the workspace's
cached endpoint listing (`state/harness/preflight/served-<profile>.json`, 24h TTL). Off, cold or stale reads as today's
static menu: routing never touches the network.

- **Refresh:** `make install`, `doctor --refresh`, `preflight --live`; with the switch on, also `switch`,
  `<agent> profile`, `configure` exit (pin changed or switch flipped on) and a detached `preflight --refresh` at
  session start, at most once per 15 minutes.
- **Fallback ignores the tier:** a swap takes the first READY entry in menu order, so a sonnet dispatch can land on
  an opus endpoint. Menu order is the control. An explicit pin swaps too; the swap appears in `gateway router` skips.
- **Live rescue:** a codex dispatch that gets `ENDPOINT_NOT_FOUND` marks the model unserved for 1h and retries once
  on the next READY Responses model. An exact `--model` never retries. copilot and opencode refuse unknown models
  client-side, so there is no captured failure text for them.
