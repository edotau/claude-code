---
paths:
  - "rules/**"
---
# Rules

Rules are standing gotchas and corrected behavior; skills (`skills/<name>/SKILL.md`) hold the how-to. Claude Code
auto-loads `~/.claude/rules/**/*.md`: **a file without `paths:` frontmatter enters every session's system prompt**,
so every line of an unscoped rule costs context on every turn. Add `paths:` to any new rule unless it must apply
to every turn:

```yaml
---
paths:
  - "**/*.py"      # loads only when a matching file is in play
---
```

## Tiers

| Tier | What it is | Loaded |
|------|-----------|--------|
| `standards/` | Coding doctrine — communication · quality · worktree-git · security · documentation | always-on (keep short), except the scoped `documentation.md` |
| `workflow/` | Process — master-workflow · agents · memory-bank · memory-consolidation | scoped to commands/agents/workflows/memory |
| `harness/` | This repo's machinery — hooks · settings-management · providers · network · agent · docs-placement · change-protocol | scoped to `internal/**` and the files each governs |
| `software/<lang>/` | Language overlays that extend `standards/` | scoped to that language's files |

## Language overlays

A `software/<lang>/<topic>.md` opens with `> This file extends [standards/<topic>.md](../../standards/<topic>.md)`.
Where idioms conflict, **the language rule wins**.

## Writers and reach

- `claude-code memory remember` then `memory promote <id> --to rules` routes a session correction into the matching
  `standards/` or `software/` file — thin entries, imperative voice. `memory dream` promotes only gated hard rules
  to `CLAUDE.md`.
- Other agent CLIs see rules only through `CLAUDE.md` prose (`claude-code agents sync`); `workflow/` and `harness/`
  are not bridged.

## Moving a rule

A rename or move breaks the overlay "extends" links, the `CLAUDE.md` index and any skill or command citing the
file — grep and update them in the same commit.
