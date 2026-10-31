---
paths:
  - "internal/settings/**"
  - "**/settings.json"
  - "**/settings.local.json"
  - "defaults/claude/settings.json"
---
# Settings Management

| To… | Run |
| --- | --- |
| Build, install, render `settings.json` | `make install` (`claude-code install --dry-run` previews) |
| Render settings + agent configs | `claude-code configure --restore [--out f] [--check] [--dry-run] [--agents]` |
| Change permissions / env / statusLine | edit `defaults/claude/settings.json` → `configure --restore --dry-run` → `make install` |
| Add or retime a hook | handler in `internal/hooks/`, register in a `settings/hookspec.go` group, `make install` (`hooks.md`) |
| Lint and auto-fix | `make lint [WHAT=go\|md\|all] [AGENT=on\|<agent>\|off] [MAX=3]`; `on` means Claude; env `HARNESS_LINT_AGENT` sets the default (`commands/lint-fix.md`) |

**Never edit `~/.claude/settings.json`.** `claude-code configure --restore` (and `make install`) renders it from
`defaults/claude/settings.json` (tracked), the vault overlay and the hook chain in
`internal/settings/hookspec.go`; a direct edit is discarded on the next render. That covers `env`, hooks,
permissions, `apiKeyHelper`, `statusLine`.
The defaults derive paths from `CLAUDE_CONFIG_DIR`/`CLAUDE_PROJECT_DIR` — never hardcode a machine path there.

1. Edit `defaults/claude/settings.json` (hooks: `hooks.md`). Confirm a new field's name and shape against the schema
   first (<https://www.schemastore.org/claude-code-settings.json>, indexed at
   `internal/settings/claude-code-settings.schema.json`). A new plain-value key also needs an entry in
   `internal/settings/catalog.go` (a test enforces it).
2. Preview: `claude-code configure --restore --dry-run`.
3. `make install` (backs up the old file to `state/harness/backups/settings.json.bak-<timestamp>`), then
   `claude-code doctor`.

`configure --restore --check` fails on invalid JSON, a key install would rewrite, a foreign key (including ones
`/config` writes) and a nested `.claude/.claude`.

## Where an override goes

- **Per-machine plain value** (e.g. `theme`) — `claude-code configure` → "Save as default": stored in the vault
  (`overlay/settings`), applied on every render. Team-wide or provider/infra config → the repo defaults.
- **`~/.claude/settings.local.json` is not a user-wide layer** — Claude Code reads it only as the project-local file
  for sessions started in `$HOME`. Use it for one-offs there, never for a global preference.

## Worktree render

cwd does not isolate writes (CLAUDE.md hard rule 4). `CLAUDE_CONFIG_DIR="$PWD" claude-code configure --restore --out
"$PWD/settings.json"` renders that checkout only. A non-default config dir skips agent renders, previews and drift
checks; `--agents` opts them back in but still targets the live `~/.codex`, `~/.copilot`, `~/.config/opencode` and
`~/.gemini` homes, so leave it out of worktree-only verification. `make install` without the override still
installs into the live harness.

## MCP

`claude-code network mcp` is read-only diagnosis (health, scope conflicts, inline secrets). Propose the fix
(`claude mcp remove <name> -s <scope>`, or move the token to `${ENV}` via `claude-code vault set`); never remove or
rotate without the user's confirmation.
