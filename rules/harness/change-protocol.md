---
paths:
  - "internal/**"
  - "agents/**"
  - "commands/**"
  - "rules/**"
  - "skills/**"
---
# Harness Change Protocol — Decide Impact, Rebuild, and Verify

> Companion to `settings-management.md` (edit the template, not the output) and `hooks.md`
> (hooks are declared in Go, not in the rendered settings file). This rule is the front door:
> run its checklist on **every** harness change before calling the change done.

## Rule

**No change to `internal/hooks/`, `internal/hookspec/`, `internal/settings/`, `agents/`,
`commands/`, `rules/`, or `skills/` is complete until you have run the Impact Checklist below
and acted on every box that applies.** The failure mode this prevents: a settings/hook change
made directly in `settings.json` that is silently erased on the next `make install` regenerate.

## Impact Checklist

1. **Settings or hooks touched?** If the change alters `env`, the hook chain, permissions, or
   `statusLine`:
   - Edit the **template** (`internal/settings/settings.tmpl.json`) and, for hooks, the
     **registry** (`internal/hookspec/hookspec.go`) + handler (`internal/hooks/`).
     **Never hand-edit `settings.json`** (it is a generated artifact).
   - Rebuild: `make install`.
   - Verify: `grep -n "<the key you changed>" ~/.claude/settings.json`, then `claude-code doctor`.
   - This is the **most important** box — a template miss means the change does not survive
     the next regenerate.

   In a linked worktree, follow CLAUDE.md hard rule 4: cwd does not isolate writes.
   `CLAUDE_CONFIG_DIR="$PWD" claude-code generate --out "$PWD/settings.json"` renders that
   checkout's settings only. With a non-default config dir, `generate` skips vendor configs
   (including `--check`/`--dry-run`) unless `--vendors` is explicit; that flag still targets
   live vendor homes. Do not add it to a worktree-only verification.

2. **Agent or command renamed/moved?** Agents are flat at `agents/<name>.md` with `name:`
   equal to the file stem — a mismatch silently breaks memory resolution. Commands keep their
   `namespace/command.md` path; a rename breaks every `/namespace:command` reference to it in
   other commands/agents/rules — grep for the old path and update every caller in the same
   commit.

3. **Rule added or moved?** A new rule under `rules/workflow/` or `rules/harness/` needs
   `paths:` frontmatter scoping it, or it loads into every session's system prompt
   (`rules/README.md`). Moving a rule breaks any hard-coded cross-reference to its old path —
   grep and fix those in the same commit.

4. **Cross-tool render stale?** If `CLAUDE.md` or the agent roster changed, regenerate vendor
   projections with `claude-code agents sync` (for example, `~/.codex/AGENTS.md` and
   `~/.gemini/GEMINI.md`; local renders are under `state/projections/`).

5. **Record the decision.** In the commit message, state what was rebuilt/regenerated and
   what was intentionally left for a later pass.

## Verification

```bash
# Settings change actually landed in the generated file
grep -n "<changed key>" ~/.claude/settings.json

# Every registered hook is wired, and nothing orphaned
claude-code doctor

# Cross-tool renders are current
claude-code agents sync --check
```

## Related

- `settings-management.md` — the template is the source of truth for `settings.json`
- `hooks.md` — hook lifecycle reference
- `rules/README.md` — the `paths:` scoping contract for new rules
