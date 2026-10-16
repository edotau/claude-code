---
paths:
  - "internal/settings/**"
  - "**/settings.json"
  - "**/settings.local.json"
  - "**/settings.tmpl.json"
---
# Settings Management

> `.claude/settings.json` and `.claude/settings.local.json` are **generated artifacts**.
> Edit the template, not the output.

## Rule

**Never edit `.claude/settings.json` directly.**

The file is rendered by `claude-code generate` (alias: `settings`; and baked in at `claude-code install`) from the
template `internal/settings/settings.tmpl.json`. Any direct edit to `settings.json` is discarded
on the next regenerate. This covers the env block, the hook chain, permission allowlists,
`apiKeyHelper`, and `statusLine` — the fields the template owns.

Only `settings.local.json` is safe to hand-edit — it holds per-workstation preferences
(`theme`, notification toggles) that a regenerate should never clobber.

## Correct workflow

1. Edit `internal/settings/settings.tmpl.json` (hooks: also edit
   `internal/hookspec/hookspec.go` first — see `hooks.md`).
2. Preview without a rebuild: `claude-code generate` prints the render.
3. `make install` — rebuilds the binary and renders the real `settings.json` (backing up the
   previous one under `backups/`).
4. `claude-code doctor` — confirm binaries, providers, and hooks are healthy after the change.

## Why

- **Idempotence**: every workstation regenerates the same config from the same template.
- **Portability**: the template computes paths from `CLAUDE_CONFIG_DIR`/`CLAUDE_PROJECT_DIR`
  dynamically, so the same template works across machines without hardcoded paths.
- **Audit trail**: template changes go through git; direct `settings.json` edits are
  indistinguishable from drift and are gitignored anyway.

## Per-workstation-only overrides

`settings.local.json` is gitignored. Beyond the seeded core scalars, edit it directly for
strictly-local one-offs (extra permissions, experimental env vars). Provider-specific/infra
durable config still belongs in the **template**, not here.

## Schema reference

When customizing settings — adding a new field, checking a valid value, or understanding what a
key does — consult the published JSON schema:

- **<https://www.schemastore.org/claude-code-settings.json>**

Use it to confirm a field name and shape **before** adding it to
`internal/settings/settings.tmpl.json` — the template is hand-written, so the schema is the
authority on what Claude Code actually accepts.

## Related

- `internal/settings/settings.tmpl.json` — the template (source of truth)
- `hooks.md` — hook lifecycle reference
- `README.md` → Install
