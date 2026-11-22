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
| Build and install (clean tracked tree required; `FORCE=1` overrides) | `rtk make install` (`claude-code install --dry-run` previews); prints `kept (yours) · valid` or the validity error |
| Reset settings.json to the clean template | `claude-code configure --restore` (owner-run; `settings.json.bak.<timestamp>` first) |
| Re-render agent configs only | `claude-code configure --restore --agents` (or `--agent X`); never touches settings.json |
| Add or retime a hook | handler in `internal/hooks/`, register in a `settings/hookspec.go` group (`hooks.md`) |
| Lint and auto-fix | `rtk make fmt [WHAT=go\|md\|js\|all]` fixes and then prints what lint still finds (main session only), `rtk make lint [SKIP=<kind>,…]` only checks and writes nothing (a leaf runs `claude-code agents diff --check --path <files>` and `agents fix --path <files>` on its own files from the output) |

## Where settings come from

- A fresh install (file missing) seeds it from `internal/settings/template.json` plus the hook chain in
  `internal/settings/hookspec.go`. `make install` never writes an existing file; its only audit is validity.
- `claude-code install` refuses when `bin/.hooks` is missing; build both binaries with `make compile` first.
- `bin/.build-stamp` is shared by both binaries; a mismatch makes `.hooks` hand over to `bin/claude-code`.
- The launch overlay (`--settings`) fills template `env` keys missing from `settings.json`; a key you set always wins.
- Host tools live in `internal/agent/tools/deps.go`; `install --tools --missing` (run by `make install`) installs
  only absent ones through `go install`, npm, uv/pipx or, as the fallback, brew and prints ok / installed / skipped
  per tool; a bare `install --dry-run` lists them. A skip or failed install never fails `make install`, no root
  command runs (apt is a printed hint), and `doctor` shows a `dep <name>` row.
- `configure --restore` is the one reset (backup, then the clean render); `--check` and `--dry-run` are refused. A hook
  change in `hookspec.go` reaches an existing file only through it or a hand edit.
- An existing `settings.json` needs the async Bash `PostToolUse` entry added by hand after `make install`, never
  before the new binary is installed (an older one rejects `--async` and the Bash hooks stop running): keep `Bash`
  in the sync `PostToolUse` matcher (the relay hand-back nudge runs there, about 7 ms) and add this entry. Without it
  `docs-plan-tick` and `worktree-claim` do not run on Bash calls.

  ```json
  {"matcher": "Bash", "hooks": [{"type": "command", "async": true, "timeout": 15,
    "command": "$HOME/.claude/bin/.hooks PostToolUse --async"}]}
  ```

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
