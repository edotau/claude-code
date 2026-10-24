---
paths:
  - "internal/memory/**"
  - "skills/reflection/**"
---
# Memory Consolidation — Post-Session Hygiene Pipeline

> Canonical rules reference for `claude-code memory update` and `memory dream`. The operational
> contract lives at `skills/reflection/SKILL.md` (verbs print their own embedded procedure); this
> file codifies the gates and scope constraints so agents can reason about the workflow without
> re-reading them. If the two drift, the skill is the source of truth and this doc catches up.

## Verbs

| Verb                              | Session Narrative | Dream Consolidation | Edits files? | Use When                                  |
| --------------------------------- | ------------------ | --------------------- | ------------ | ------------------------------------------ |
| `claude-code memory update`      | yes                 | no                     | yes          | End of a working session                  |
| `claude-code memory dream`       | no                  | yes                    | yes          | Periodic hygiene; context-window pressure |
| `claude-code memory dream --dry-run` | no              | audit-only             | **no**       | Plan before a real dream run              |

## Pipeline Overview

```text
[SESSION NARRATIVE — claude-code memory update]
  SESSION SUMMARY --> REWRITE ACTIVECONTEXT/PROGRESS --> DECISIONS --> CONVENTIONS --> SETTLE

[DREAM CONSOLIDATION — Sonnet subagent owns Cross-Pollinate — claude-code memory dream]
  BASELINE --> AUDIT --> REVIEW RECENT SESSIONS --> PRUNE NATIVE MEMORY
    --> REFRESH BANK --> CROSS-POLLINATE --> REPORT
```

## The promotion gradient

```text
bank/native entry ──promote --to home──▶ home bank (~/.claude/memory/home/)
                   ──promote --to rules─▶ rules/standards/<topic>.md
                                          rules/software/<lang>/<topic>.md
                   ──promote --to skill─▶ skills/<name>/SKILL.md
                                                  │ dream gate only:
                                                  │ 3+ memory entries AND
                                                  │ hard prohibition
                                                  ▼
                                          CLAUDE.md "Known Gotchas"   ← always-on tier
                                          (memory dream only, gated; no direct --to claude-md)
```

Each step up costs more context. `CLAUDE.md` is the deliberate always-on tier, reached only
through the gate.

## The two writers

- **`claude-code memory remember <fact>` then `memory promote <id> --to rules`** — a correction is first
  captured as a memory entry, then promoted into the matching `rules/standards/` or `rules/software/` file
  (routing table printed by `memory promote --to rules`'s embedded procedure). It **never** edits
  `rules/workflow/`, and never edits `CLAUDE.md`.
- **`claude-code memory dream`** — curates native project memory and (gated) promotes hard prohibitions
  up to `CLAUDE.md`. It **never** edits `rules/**` beyond the gated promotion — those files are
  human/`adapt`-authored and git-tracked.

## Scope constraints

- `claude-code memory <verb>` is the single entry point — it curates both Claude
  Code's native project memory and the typed bank files, in the same `~/.claude/memory/<repo>/`
  dir (the harness itself is `memory/claude-code/`; `~/.claude/projects/<slug>/memory` is a symlink
  into it). There is no separate "learnings" feedstock file in this harness; a correction either
  routes straight to a `rules/` file (via `memory promote --to rules`) or stays in
  native memory until it does.
- Native memory edits never touch the approval-gated RULES region
  (`<!-- RULES:BEGIN -->`…`<!-- RULES:END -->`) without explicit human approval.
- `sessionHistory.md` caps at 30 entries (overflow archives via `claude-code memory index`);
  `conventions.md`/`decisionLog.md` stay in scope (no automatic promotion to home); move
  durable ones out by hand via `memory promote --to home`.

## Triggering

Event-driven, not scheduled: run `claude-code memory update` at the end of a session, when context
pressure is high (≥85% window — the `context-checkpoint` Stop hook nudges a handoff
(`memory update`) or `/compact`), or periodically (`memory dream`) for hygiene.

## Related

- `skills/reflection/SKILL.md` — the operational contract (verb table; source of truth on drift)
- `rules/workflow/memory-bank.md` — the bank's file contracts and caps
