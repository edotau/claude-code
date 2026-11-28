---
paths:
  - "internal/platform/settings/**"
  - "internal/routing/agentconfig/*.go"
  - "**/settings.json"
---
# Settings Management

Ownership and reset: `CLAUDE.md` hard rule 1.

`configure fmt` sorts keys to schemastore order, env keys and permission lists; other lists and values stay intact.
It keeps a backup in `state/harness/backups/`; explicit `--fmt --check --out <file>` stays strict.

- Hand-edit permissions, env, statusLine and hooks in `~/.claude/settings.json`, then run `claude-code doctor`.
- Vault env: `claude-code vault set env NAME VALUE`, `vault rm env NAME`, `vault ls env`.
  Replacements ask; `--yes` skips the ask and is required headless.
- Build/install the current tree: `rtk make install`; preview with `claude-code install --dry-run`.
- Owner-run reset: `claude-code configure restore`, with a timestamped backup first; no check/dry-run mode.
  `--agent all` or `--agent X` renders only agent configs.
- Hook changes: `hooks.md`; format/lint and leaf-scoped fixes: `rules/standards/quality.md` → Verification Order.

## Where settings come from

- A missing file is seeded from `internal/assets/settings/template.json` and `internal/platform/settings/hookspec.go`.
  Install only validates an existing file; hook changes reach it by hand edit or explicit restore.
- Build both binaries with `make compile` first: install requires `bin/.hookd`.
  Both share `bin/.build-stamp`; a mismatch hands hooks to `bin/claude-code`.
- Launch `--settings` fills missing template env keys and missing top-level keys whole; an existing key always wins.
- `modelSettings` (effort, max effort, auto-compact window) comes from `models.ModelSettings` at launch,
  keyed by session Claude model ids, never from `settings.json`.
- Host tools: `internal/agent/tools/deps.go`. Install runs `install --tools --missing` using go/npm/uv/pipx,
  then brew as fallback. No root commands; apt is a hint. Skips/failures do not fail install; doctor reports each tool.
- Add the async Bash PostToolUse entry by hand only after installing the supporting binary; keep Bash in the sync
  matcher for dispatch hand-back. Bash plan ticks and claims need the async entry:

  ```json
  {"matcher": "Bash", "hooks": [{"type": "command", "async": true, "timeout": 15,
    "command": "$HOME/.claude/bin/.hookd PostToolUse --async"}]}
  ```

- Derive paths from `CLAUDE_CONFIG_DIR`/`CLAUDE_PROJECT_DIR`, never machine paths.
- Check new fields against `internal/assets/settings/claude-code-settings.schema.json`
  (<https://www.schemastore.org/claude-code-settings.json>).
- Vendor CLI configs have no edit guard; change them only for the task.
- Show proposed `permissions.allow` additions in a table and wait for the owner's pick, including
  `/fewer-permission-prompts`.

## Fullscreen mouse settings (change together)

Wheel scrolling and selection are decided by four keys read as one set; changing one alone flips the others' effect.
Source: <https://code.claude.com/docs/en/fullscreen> → Keep native text selection, Mouse wheel scrolling.

| Key | Where |
| --- | --- |
| `tui` (`fullscreen`) | `~/.claude/settings.json` |
| `CLAUDE_CODE_DISABLE_MOUSE`, `CLAUDE_CODE_DISABLE_MOUSE_CLICKS`, `CLAUDE_CODE_SCROLL_SPEED` | its `env` block |
| `terminal.integrated.macOptionClickForcesSelection`, `copyOnSelection`, `mouseWheelScrollSensitivity` | VS Code user settings, merged by `claude-code configure vscode` |

| `env` in fullscreen | Wheel scrolls the conversation | Plain drag | Terminal's own highlight |
| --- | --- | --- | --- |
| neither variable | yes | Claude Code's selection, copies on release | Option-drag (macOS), Shift-drag |
| `CLAUDE_CODE_DISABLE_MOUSE_CLICKS=1` | yes | nothing | Option-drag (macOS), Shift-drag |
| `CLAUDE_CODE_DISABLE_MOUSE=1` (wins over the other) | no; VS Code sends the wheel as Up/Down keys | terminal's | plain drag |

- No set gives wheel scrolling and the terminal's highlight on a plain drag: the wheel needs mouse capture, and
  capture hands a plain drag to Claude Code.
- Option-drag needs `macOptionClickForcesSelection: true`. `mouseWheelScrollSensitivity` has no effect in fullscreen;
  `CLAUDE_CODE_SCROLL_SPEED` sets the wheel distance and only applies while the mouse is captured.
- A new session reads the set at launch; a running one keeps its own.

## Worktree render

cwd does not isolate writes. Reset only this checkout with
`CLAUDE_CONFIG_DIR="$PWD" claude-code configure restore --out "$PWD/settings.json"`.
A non-default config dir skips agent renders; explicit `--agent` targets live agent homes and cannot combine with
`--out`. Unoverridden `make install` still installs into the live harness.
