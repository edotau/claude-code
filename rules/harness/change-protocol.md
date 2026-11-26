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

Act on every applicable item when changing hooks, settings, agent configs, agents, commands, rules, skills or workflows.

## Impact Checklist

1. **Settings or hooks:** follow `settings-management.md` and `hooks.md`; hand-edit existing settings, run
   `claude-code doctor`, and install hook/spec changes with `rtk make install` for native projections.
2. **Agent or command moved/renamed:** follow `rules/workflows/agents.md` → Agent and command files;
   search the old name and fix every caller in the same change.
3. **Rule added/moved:** new `workflows/` and `harness/` rules need `paths:`; unscoped `standards/*.md`
   are the always-on core. Fix overlay extends links, the `CLAUDE.md` index, skills and commands in the same change.
4. **Skill, CLAUDE.md or roster changed:** run `claude-code agents sync`, then `claude-code agents sync --check`.
   If projection roots are unwritable, report the boundary and exact pending command.
5. **File writer or disk format changed:** register/classify it in `internal/schemas`, including
   `testdata/writers/<pkg>.txt` and the registry golden. Refresh hook/settings goldens and examples in the same change.
6. **Record:** the commit message names what was rebuilt and deferred.
7. **Component added/changed (agent, skill, hook, plugin):** write a manifest of files/actions and get approval first.
8. **Saved workflow changed:** run `claude-code workflow lint`; update the saved-workflows reference table,
   keep `stepOf` verbatim and ensure every script parses under node.
9. **Instruction path changed:** run `claude-code doctor --prompts` and fix findings at their origin. This covers
   rules, skills, CLAUDE.md, plan templates, workflows, agents and session-start/recall hooks.

Check generated files for leaked placeholders:

```bash
rtk grep -nE '<(agent|files|slug|verb|object)>|\$\{CLAUDE_PLUGIN_ROOT\}|__CLAUDE_BIN__|TODO|\{PLACEHOLDER\}' <files>
```
