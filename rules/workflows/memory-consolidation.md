---
paths:
  - "internal/memory/**"
  - "skills/reflection/**"
---
# Memory Consolidation

Gates and scope for `claude-code memory update` and `memory dream`. Precedence, store layout, caps and verbs:
`memory-bank.md`; the promotion ladder: `skills/reflection/SKILL.md`.

| Verb | Does | Edits files? | When |
| --- | --- | --- | --- |
| `memory update` | Session narrative: summary → activeContext/progress → decisions → conventions → settle | yes | Session end; ≥85% context (`memory-update`'s checkpoint nudges it) |
| `memory dream` | Consolidation (on Claude, the logged-in user): baseline → audit → recent sessions → prune native memory → refresh bank → cross-pollinate → report | yes | Periodic hygiene, context pressure |
| `memory dream --dry-run` | Audit only | **no** | Before a real dream |

## Promotion gradient

The ladder lives in `skills/reflection/SKILL.md` → Promotion ladder. `--to claude-md` is refused outside
`memory dream`'s own run; inside it, it prints the block like `rules` and `skill` do.

## Writer limits

- `memory promote --to rules` targets only `rules/standards/` or `rules/software/` — **never** `rules/workflows/` or
  `CLAUDE.md`.
- `memory dream` never edits `rules/**`; its only `CLAUDE.md` edit is a block `promote --to claude-md` printed in its
  own run.
- Native memory edits never touch the `<!-- RULES:BEGIN -->`…`<!-- RULES:END -->` region without human approval.
- `conventions.md`/`decisionLog.md` entries never auto-promote to home; move durable ones with `memory promote --to
  home`.
