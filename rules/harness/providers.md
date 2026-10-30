---
paths:
  - "internal/providers/**"
  - "internal/vault/**"
  - "internal/models/**"
  - "internal/proxy/**"
  - "internal/router/**"
  - "internal/secrets/**"
  - "internal/keyring/**"
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
- Reference model aliases (`opus`/`sonnet`/`haiku`), never concrete ids, in prose (`rules/standards/performance.md`).
