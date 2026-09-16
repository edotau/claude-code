---
paths:
  - "rules/**"
---
# Rules

Standing policy for every Claude Code session on this machine. Claude Code auto-loads
`~/.claude/rules/**/*.md`: **a file without `paths:` frontmatter enters every session's system
prompt**, so a rule is always-on by default and `paths:` is what makes it on-demand:

```yaml
---
paths:
  - "**/*.py"      # loads only when a matching file is in play
---
```

Add `paths:` to any new rule unless it must apply to every turn.

## Tiers

| Tier | What it is | Loaded |
|------|-----------|--------|
| `standards/` | Coding doctrine — how code is written, reviewed, committed, documented, secured | always-on (portable, short) |
| `workflow/` | Process — the end-to-end development pipeline | scoped to commands/agents/workflows |
| `software/<lang>/` | Language overlays that **extend** `standards/` where idioms differ | scoped to that language's files |

```
rules/
├── standards/        communication · quality · git · documentation · security
├── workflow/         master-workflow
└── software/python/  coding-style · testing · patterns · security   (extend ../../standards/*)
```

## Language overlays

A `software/<lang>/<topic>.md` file opens with `> This file extends
[standards/<topic>.md](../../standards/<topic>.md) …` and adds language-specific content. Where
idioms conflict with the general standard, **the language rule wins** (specific overrides general).

## Moving a rule

Renaming or moving a rule breaks hard-coded paths: the `software/<lang>/` "extends" links, the
`CLAUDE.md` index, and any skill or command that cites the file. Update them in the same commit.

## Rules vs skills

- **Rules** = standing standards and process that apply broadly. They say *what*.
- **Skills** (`skills/<name>/SKILL.md`) = deep, on-demand how-to reference, triggered by
  description keywords. They say *how*.
