---
paths:
  - "internal/hooks/**"
  - "internal/settings/**"
  - "internal/agentconfig/**"
  - "agents/**"
  - "commands/**"
  - "rules/**"
  - "skills/**"
  - "workflows/**"
---
# Harness Change Protocol

**A change to `internal/hooks/`, `internal/settings/`, `internal/agentconfig/*.go`, `agents/`, `commands/`, `rules/`,
`skills/` or `workflows/` is not done until every applicable box below is acted on.** The failure it prevents: a hook,
rule or agent change that never reaches the sessions and agents that load it.

## Impact Checklist

1. **Settings or hooks** — edit `~/.claude/settings.json` by hand (`settings-management.md`, `hooks.md`), then
   `claude-code doctor`; a `hookspec.go` or `internal/hooks/` change also needs `rtk make install` for native
   projections.
2. **Agent or command renamed/moved** — layout rules in `rules/workflows/agents.md` → Agent and command files (a `name:`
   that differs from the file stem fails `agents sync` and `doctor --prompts`); grep the old name and fix every caller
   in the same commit.
3. **Rule added or moved** — a rule without `paths:` loads into every prompt, so a new `workflows/` or `harness/` rule
   needs `paths:` (unscoped `standards/*.md` are the always-on core rendered to other agents); a moved rule's
   references (overlay "extends" links, the `CLAUDE.md` index, skills, commands) are fixed in the same commit.
4. **Skill, `CLAUDE.md` or the agent roster changed** — `claude-code agents sync`, then `agents sync --check`; if
   projection roots are not writable, report that boundary and the exact pending command.
5. **New file writer or on-disk format** — register or classify it in `internal/schemas`
   (one row in `testdata/writers/<pkg>.txt`, registry golden);
   a hook or settings change regenerates its golden files and examples in the same commit.
6. **Record it** — the commit message says what was rebuilt and what was left for later.
7. **New or changed component (agent/skill/hook/plugin)** — write the manifest first (exact files, action on each) and
   get approval before writing.
8. **Saved workflow added/changed** — `claude-code workflow lint` (table row in
   `skills/subagent-workflows/references/saved-workflows.md`, `stepOf` verbatim, every script parses under node).
9. **Instruction path changed** (`CLAUDE.md`, a rule, a skill, the plan template, a saved workflow, an agent, or a
   session-start/recall hook) — `claude-code doctor --prompts` (hops H1–H7; it checks placeholder paths);
   fix the finding at its origin.

Files generated from a template: no placeholder leaked through —

```bash
rtk grep -nE '<(agent|files|slug|verb|object)>|\$\{CLAUDE_PLUGIN_ROOT\}|__CLAUDE_BIN__|TODO|\{PLACEHOLDER\}' <files>
```
