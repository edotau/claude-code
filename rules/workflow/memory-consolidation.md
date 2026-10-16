---
paths:
  - "internal/memory/**"
  - "commands/memory/*.md"
  - "commands/reflection/*.md"
---
# Memory Consolidation — Post-Session Hygiene Pipeline

> Canonical rules reference for `/reflection:consolidate` and `/reflection:consolidate --dream`.
> The operational command lives at `commands/reflection/consolidate.md`; this file codifies the
> gates and scope constraints so agents can reason about the workflow without re-reading the
> command. If the two drift, the command is the source of truth and this doc catches up.

## Modes

| Invocation                          | Session Reflection | Dream Consolidation | Edits files? | Use When                                  |
| ------------------------------------ | ------------------- | --------------------- | ------------ | ------------------------------------------ |
| `/reflection:consolidate`           | yes                  | yes (auto)             | yes          | End of a working session                  |
| `/reflection:consolidate --dream`    | skip                 | yes                    | yes          | Periodic hygiene; context-window pressure |
| `/reflection:consolidate --dry-run`  | skip                 | audit-only             | **no**       | Plan before a real dream run              |

## Pipeline Overview

```
[SESSION REFLECTION — skipped in --dream and --dry-run]
  SESSION SUMMARY --> UPDATE AGENT MEMORIES --> UPDATE MEMORY BANK --> ROUTE CORRECTIONS TO RULES

[DREAM CONSOLIDATION — Sonnet subagent owns Cross-Pollinate]
  BACKUP --> AUDIT --> REVIEW RECENT SESSIONS --> PRUNE AGENT MEMORIES
    --> REFRESH BANK --> CROSS-POLLINATE --> REPORT
```

## The promotion gradient

```
agent memory ──promote (/reflection:promote)──▶ rules/standards/<topic>.md
                                                 rules/software/<lang>/<topic>.md
                                                          │ dream gate only:
                                                          │ 3+ agent memories AND
                                                          │ hard prohibition
                                                          ▼
                                          CLAUDE.md "Known Gotchas"   ← always-on tier
                                          (/reflection:consolidate --dream only, gated)
```

Each step up costs more context. `CLAUDE.md` is the deliberate always-on tier, reached only
through the gate.

## The two writers

- **`/harness:adapt`** — captures a session's corrections and routes each into the matching
  `rules/standards/` or `rules/software/` file (routing table in `commands/harness/adapt.md`).
  It **never** edits `rules/workflow/`, and never edits `CLAUDE.md`.
- **`/reflection:consolidate --dream`** — curates agent memories and (gated) promotes hard
  prohibitions up to `CLAUDE.md`. It **never** edits `rules/**` beyond the gated promotion —
  those files are human/`adapt`-authored and git-tracked.

## Scope constraints

- `/reflection:*` curates Claude Code's native agent memory (`agent-memory/<name>/`) and the
  session memory bank (`docs/memory/bank/`). It does not invent a third tier — there is no
  separate "learnings" feedstock file in this harness; a correction either routes straight to a
  `rules/` file (via `/harness:adapt` or `/reflection:promote`) or stays in agent memory until it
  does.
- Agent-memory edits never touch the approval-gated RULES region
  (`<!-- RULES:BEGIN -->`…`<!-- RULES:END -->`) without explicit human approval.
- `sessionHistory.md` caps at 30 entries (overflow archives via `claude-code memory index`);
  `conventions.md`/`decisionLog.md` overflow promotes to the global bank via `claude-code memory
  promote`.

## Triggering

Event-driven, not scheduled: run `/reflection:consolidate` at the end of a session, when context
pressure is high (>80% window — the `context-checkpoint` Stop hook nudges `/harness:adapt`
first, a lighter sibling of this pipeline), or periodically (`--dream`) for hygiene.

## Related

- `commands/reflection/consolidate.md` — the operational command (source of truth on drift)
- `commands/harness/adapt.md` — the lighter, faster correction-capture checkpoint
- `rules/workflow/memory-bank.md` — the bank's file contracts and caps
