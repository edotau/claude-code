---
paths:
  - "internal/**"
  - "agents/**"
  - "commands/**"
  - "rules/**"
  - "skills/**"
---
# Harness Change Protocol

**A change to `internal/hooks/`, `internal/hookspec/`, `internal/settings/`, `agents/`, `commands/`, `rules/` or
`skills/` is not done until every applicable box below is acted on.** The failure it prevents: an edit made in
the generated `settings.json`, silently erased by the next `make install`.

## Impact Checklist

1. **Settings or hooks** (`env`, hook chain, permissions, `statusLine`) — edit `defaults/claude/settings.json` (hooks:
   `hooks.md`), `make install`, then `grep -n "<key>" ~/.claude/settings.json` and `claude-code doctor`. In a linked
   worktree, cwd does not isolate writes: `settings-management.md` → worktree render.
2. **Agent or command renamed/moved** — agents are flat `agents/<name>.md` with `name:` equal to the stem (a mismatch
   silently breaks memory resolution); commands are flat `commands/<command>.md`. Grep the old name and fix every
   caller in the same commit.
3. **Rule added or moved** — a new `workflow/` or `harness/` rule needs `paths:` or it loads into every prompt
   (`rules/README.md`); grep and fix references to a moved rule's old path.
4. **`CLAUDE.md` or the agent roster changed** — `claude-code agents sync` (check: `agents sync --check`).
5. **New file writer or on-disk format** — register or classify it in `internal/schemas` (writers map, registry
   golden); a hook or settings change regenerates its golden files and examples in the same commit.
6. **Record it** — the commit message says what was rebuilt and what was left for later.
7. **New or changed component (agent/skill/hook/plugin)** — write the manifest first (exact files, action on each)
   and get approval before writing.

Files generated from a template: no placeholder leaked through —

```bash
grep -nE '<(agent|files|slug|verb|object)>|\$\{CLAUDE_PLUGIN_ROOT\}|__CLAUDE_BIN__|TODO|\{PLACEHOLDER\}' <files>
```
