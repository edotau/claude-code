---
paths:
  - "internal/settings/**"
  - "internal/agentconfig/render_*.go"
  - "**/settings.json"
  - "internal/settings/template.json"
---
# Settings Management

`~/.claude/settings.json` is the owner's one settings file: hand-edited, untracked. Nothing else writes it once it
exists.

| To… | Run |
| --- | --- |
| Change permissions / env / statusLine / hooks | edit `~/.claude/settings.json` by hand, then `claude-code doctor` (`settings.json: valid`) |
| Build and install | `make install` (`claude-code install --dry-run` previews); prints `kept (yours) · valid` or the validity error |
| Reset settings.json to the clean template | `claude-code configure --restore` (owner-run; `settings.json.bak.<timestamp>` first) |
| Re-render agent configs only | `claude-code configure --restore --agents` (or `--agent X`); never touches settings.json |
| Add or retime a hook | handler in `internal/hooks/`, register in a `settings/hookspec.go` group (`hooks.md`) |
| Lint and auto-fix | `make lint [WHAT=go\|md\|all] [AGENT=on\|<agent>\|off] [MAX=3]`; `on` means Claude Sonnet at medium effort; env `HARNESS_LINT_AGENT` sets the default |

## Where settings come from

- **Fresh install** (file missing) seeds it from `internal/settings/template.json` (embedded: `$schema`,
  `env.CLAUDE_CODE_PLUGIN_DIRS`, `permissions`, `statusLine`) plus the hook chain in `internal/settings/hookspec.go`.
- **`make install` never writes an existing file.** Its only audit is validity (JSON parses, schema values).
- **`configure --restore`** is the one reset: backup, then the clean render. `--check` and `--dry-run` are refused.
- A hook change in `hookspec.go` reaches an existing file only through `configure --restore` or a hand edit.
- Paths derive from `CLAUDE_CONFIG_DIR`/`CLAUDE_PROJECT_DIR` — never hardcode a machine path.
- Confirm a new field's name and shape against the schema first
  (<https://www.schemastore.org/claude-code-settings.json>, indexed at
  `internal/settings/claude-code-settings.schema.json`).
- **Propose permission additions first:** show new `permissions.allow` rules as a table and wait for the owner's pick
  before editing (`/fewer-permission-prompts` included).

## Worktree render

cwd does not isolate writes (CLAUDE.md hard rule 4). `CLAUDE_CONFIG_DIR="$PWD" claude-code configure --restore --out
"$PWD/settings.json"` resets that checkout only. A non-default config dir skips agent renders; `--agents` opts them
back in but still targets the live `~/.codex`, `~/.copilot`, `~/.config/opencode` and `~/.gemini` homes, so leave it
out of worktree-only verification. `make install` without the override still installs into the live harness.

## MCP

`claude-code doctor --mcp` is read-only diagnosis (health, scope conflicts, inline secrets). Propose the fix
(`claude mcp remove <name> -s <scope>`, or move the token to `${ENV}` via `claude-code vault set`); never remove or
rotate without the user's confirmation.
