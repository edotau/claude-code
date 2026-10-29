---
paths:
  - "internal/settings/**"
  - "**/settings.json"
  - "**/settings.local.json"
  - "defaults/claude/settings.json"
---
# Settings Management

> `.claude/settings.json` is **generated**; `settings.local.json` is hand-edited.
> Edit the defaults, not the generated output.

## Rule

**Never edit `.claude/settings.json` directly.**

The file is rendered by `claude-code configure --restore` (and baked in at `claude-code install`) from
the repo defaults file `defaults/claude/settings.json` (tracked), the per-machine vault overlay, and the hook chain
from `internal/hookspec/hookspec.go`. Any direct edit to `settings.json` is discarded on the
next regenerate. This covers the env block, the hook chain, permission allowlists, `apiKeyHelper`, and
`statusLine` — the fields the defaults own.

A per-machine override of a plain-value key (e.g. `theme`) goes through `claude-code configure` →
"Save as default", not a hand edit: it is stored (plain) in the vault (`overlay/settings`) and applied at render time,
so it survives `make install`. Team-wide defaults go in the repo defaults file ("Save to repo defaults", or by hand).

`~/.claude/settings.local.json` is **not** a user-wide settings layer — Claude Code only reads it as the
project-local file for sessions started in `$HOME`. Don't put a global preference there.

## Correct workflow

1. Edit `defaults/claude/settings.json`; for hooks: `hooks.md` → authoring surface.
2. Preview without a rebuild: `claude-code configure --restore --dry-run` prints the render.
3. `make install` — rebuilds the binary and renders the real `settings.json` (backing up the
   previous one under `state/harness/backups/` as `settings.json.bak-<timestamp>`).
4. `claude-code doctor` — confirm binaries, providers, and hooks are healthy after the change.

`configure --restore --check` fails on invalid JSON in `settings.json`, on a key install would rewrite, on a
foreign key (including keys Claude Code's `/config` writes), and on a nested `.claude/.claude`.

In a linked worktree, cwd alone does not isolate writes (CLAUDE.md hard rule 4).
Use `CLAUDE_CONFIG_DIR="$PWD" claude-code configure --restore --out "$PWD/settings.json"` for that checkout's
settings. A non-default config dir skips agent renders, previews, and drift checks by default.
`--agents` opts them back in, but destinations remain the live `~/.codex`, `~/.copilot`,
`~/.config/opencode`, and `~/.gemini` homes; `--dry-run`/`--check` still write nothing.
`make install` without the override still installs into the live harness.

## Why

- **Idempotence**: every workstation regenerates the same config from the same defaults.
- **Portability**: the defaults compute paths from `CLAUDE_CONFIG_DIR`/`CLAUDE_PROJECT_DIR`
  dynamically, so the same defaults work across machines without hardcoded paths.
- **Audit trail**: defaults changes go through git; direct `settings.json` edits are
  indistinguishable from drift and are gitignored anyway.

## Per-workstation-only overrides

`settings.local.json` is gitignored and project-local to `$HOME` only (see above); use it for
strictly-local one-offs there (extra permissions, experimental env vars), not for a preference meant to
follow you everywhere. A plain-value key that should: `claude-code configure` → "Save as default" (vault
overlay, every machine, every launch). Provider-specific/infra durable config still belongs in the
**repo defaults**.

`claude-code network mcp` is read-only diagnosis of MCP health, scope conflicts and inline secrets;
propose the fix (`claude mcp remove <name> -s <scope>`, or move the token to `${ENV}` via
`claude-code vault set`) and never remove or rotate without the user's confirmation.

## Schema reference

When customizing settings — adding a new field, checking a valid value, or understanding what a
key does — consult the published JSON schema:

- **<https://www.schemastore.org/claude-code-settings.json>**

Use it to confirm a field name and shape **before** adding it to
`defaults/claude/settings.json` — it is hand-editable, so the schema (indexed locally in
`internal/settings/claude-code-settings.schema.json`) is the authority on what Claude Code actually accepts.

A new plain-value key also needs an entry in `internal/settings/catalog.go` — a test enforces
that every such key is catalogued.

## Related

- `defaults/claude/settings.json` — the repo defaults (source of truth)
- `hooks.md` — hook lifecycle reference
- `README.md` → Install
