---
name: reflection
description: >-
  The memory bank's one entry point — `claude-code memory <verb>`; a verb that needs judgment
  prints its own embedded procedure. Promotion ladder, which verbs print one. Trigger on
  "recall memory", "remember this", "save this session", "promote this to a rule/skill",
  "memory doctor", "dream consolidation", or any `claude-code memory *` dispatch needing judgment.
user-invocable: false
---

# Reflection — The Memory Bank's Entry Point

`claude-code memory <verb>` does every mechanical step; a verb needing judgment prints its own embedded procedure for
the live session (or the headless `memory update` run) to apply. Store layout, precedence and verbs:
`rules/workflows/memory-bank.md`.

## Promotion ladder

The one copy; the other docs point here. Each step up costs more context:

1. `promote --to home` — moves the block (no link back) to the home bank (`~/.claude/memory/home/`, cross-project).
2. `promote --to rules` — prints the block for `rules/standards/<topic>.md` or `rules/software/<lang>/<topic>.md`
   (enforced); never `rules/workflows/` or `CLAUDE.md`. `promote --to skill` — prints it for
   `skills/<name>/SKILL.md` or its `references/` (how-to).
3. `promote --to claude-md` — `CLAUDE.md` → Hard rules (always-on tier). Gated: 3+ bank entries and a hard
   prohibition, so the verb runs only inside `memory dream`'s cross-pollination step and is refused anywhere else.

Only `home` writes; every other target prints the block, its target and the procedure, and the agent edits by hand.

## Verb table

Every verb and its flags: `rules/workflows/memory-bank.md` → Verbs. These print an embedded procedure:

| Verb | Prints a procedure when |
| --- | --- |
| `init` | `projectContext.md` is still the template |
| `review` | `--candidates` (the ranking procedure) |
| `promote <id> --to <target>` | `--to rules\|skill`, and `--to claude-md` inside `memory dream` |
| `update` | no session or `--transcript` resolves (live session) |
| `dream` | the headless run hands it to the model; `--dry-run` is a deterministic preview, no model |

All other verbs print no procedure.
