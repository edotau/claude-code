---
paths:
  - "internal/settings/**"
  - "internal/agentconfig/*.go"
  - "**/settings.json"
---
# Settings Management

`~/.claude/settings.json` ownership and reset: `CLAUDE.md` hard rule 1.

| To… | Run |
| --- | --- |
| Change permissions / env / statusLine / hooks | edit `~/.claude/settings.json` by hand, then `claude-code doctor` (`settings.json: valid`) |
| Set or remove a vault env value (injected into launches; not a secret) | `claude-code vault env set NAME VALUE` / `vault env unset NAME` (`vault ls env` lists) |
| Build and install | `make install` (`claude-code install --dry-run` previews); prints `kept (yours) · valid` or the validity error |
| Reset settings.json to the clean template | `claude-code configure --restore` (owner-run; `settings.json.bak.<timestamp>` first) |
| Re-render agent configs only | `claude-code configure --restore --agents` (or `--agent X`); never touches settings.json |
| Add or retime a hook | handler in `internal/hooks/`, register in a `settings/hookspec.go` group (`hooks.md`) |
| Lint and auto-fix | `make fmt [WHAT=go\|md\|js\|all] [AGENT=on\|<agent>\|off] [MAX=3]` fixes, `make lint [SKIP=<kind>,…]` checks and auto-wraps own memory banks (`memory fmt --own`); `on` means copilot (`--direct`); env `HARNESS_LINT_AGENT` sets the default |

## Where settings come from

- A fresh install (file missing) seeds it from `internal/settings/template.json` plus the hook chain in
  `internal/settings/hookspec.go`. `make install` never writes an existing file; its only audit is validity.
- `configure --restore` is the one reset (backup, then the clean render); `--check` and `--dry-run` are refused. A hook
  change in `hookspec.go` reaches an existing file only through it or a hand edit.
- Paths derive from `CLAUDE_CONFIG_DIR`/`CLAUDE_PROJECT_DIR`, never a hardcoded machine path.
- Confirm a new field's name and shape against the schema (`internal/settings/claude-code-settings.schema.json`,
  from <https://www.schemastore.org/claude-code-settings.json>) first.
- Propose new `permissions.allow` rules as a table and wait for the owner's pick before editing
  (`/fewer-permission-prompts` included).

## Worktree render

cwd does not isolate writes (`worktree-git.md` → Worktrees). `CLAUDE_CONFIG_DIR="$PWD" claude-code configure --restore
--out "$PWD/settings.json"` resets that checkout only. A non-default config dir skips agent renders; `--agents` opts
them back in but still targets the live `~/.codex`, `~/.copilot`, `~/.config/opencode` and `~/.gemini` homes, so leave
it out of worktree-only verification. `make install` without the override still installs into the live harness.
