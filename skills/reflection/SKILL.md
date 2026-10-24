---
name: reflection
description: >-
  The memory bank's one entry point — `claude-code memory <verb>`; a verb that needs judgment
  prints its own embedded procedure. Bank contract, promotion ladder, verb table. Trigger on
  "recall memory", "remember this", "save this session", "promote this to a rule/skill",
  "memory doctor", "dream consolidation", or any `claude-code memory *` dispatch needing judgment.
user-invocable: false
---

# Reflection — The Memory Bank's Entry Point

One store (`~/.claude/memory/<repo>/`, catalog at `memory/index.md`): `claude-code memory <verb>`
does every mechanical step; a verb needing judgment — text a model must produce — prints its own
embedded procedure for the live session (or the headless `memoryAgent()` runner) to apply.

## Promotion ladder

`promote --to home` → home bank (`~/.claude/memory/home/`, cross-project) → `promote --to rules|skill`
→ `rules/standards|software/<topic>.md` (enforced) or `skills/<name>/SKILL.md` (how-to) →
`--to claude-md` (dream-gated: 3+ bank entries + hard prohibition, valid only inside `memory dream`'s
cross-pollination step) → `CLAUDE.md` "Known Gotchas" (always-on tier).

## Verb table

| Verb | Prints a procedure when |
| --- | --- |
| `recall` / `remember [--type]` | never |
| `init` | `projectContext.md` is still the template |
| `review [--candidates]` | `--candidates` |
| `promote <id> --to home\|rules\|skill\|claude-md` | `--to rules\|skill` |
| `update [--transcript]` | no `--transcript` (live session) |
| `dream [--dry-run]` | never — `--dry-run` previews, no model, no writes |

Off the menu, dispatchable by hand: `index`, `watch`, `fmt`, `migrate`, `alias`, `path`, `show`,
`digest`, `timeline`.

Before a non-trivial task: `claude-code memory recall <task terms>`, then `memory show <id>`.
