---
paths:
  - "internal/memory/**"
  - "skills/reflection/**"
---
# Memory Consolidation

Gates and scope for `claude-code memory update` and `memory dream`. The operational contract is
`skills/reflection/SKILL.md` (verbs print their own procedure) — on drift the skill wins. Store layout:
`memory-bank.md`.

| Verb | Does | Edits files? | When |
| --- | --- | --- | --- |
| `memory update` | Session narrative: summary → activeContext/progress → decisions → conventions → settle | yes | Session end; ≥85% context (`memory-harvest`'s checkpoint nudges it) |
| `memory dream` | Consolidation (Sonnet): baseline → audit → recent sessions → prune native memory → refresh bank → cross-pollinate → report | yes | Periodic hygiene, context pressure |
| `memory dream --dry-run` | Audit only | **no** | Before a real dream |

## Promotion gradient

```text
entry ─promote --to home──▶ memory/home/
      ─promote --to rules─▶ rules/standards/<topic>.md · rules/software/<lang>/<topic>.md
      ─promote --to skill─▶ skills/<name>/SKILL.md
      ─promote --to claude-md / memory dream, gated (3+ entries AND a hard prohibition)─▶ CLAUDE.md
```

Each step up costs more context; `--to claude-md` only prints the block for review — nothing bypasses the gate.

## Writer limits

- `memory promote --to rules` targets only `rules/standards/` or `rules/software/` — **never** `rules/workflow/` or
  `CLAUDE.md`.
- `memory dream` never edits `rules/**`; its only `CLAUDE.md` write is the gated promotion.
- Native memory edits never touch the `<!-- RULES:BEGIN -->`…`<!-- RULES:END -->` region without human approval.
- `conventions.md`/`decisionLog.md` entries never auto-promote to home; move durable ones with `memory promote --to
  home`.
