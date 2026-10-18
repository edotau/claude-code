---
description: "Normally automatic (dream consolidation) — manual audit of native project memory: health dashboard (--brief/--quick) or a full read-only audit of promotion candidates, stale entries, and consolidation opportunities via the opencode agent (ANALYZE mode)."
argument-hint: "[--brief | --quick | --stale | --candidates]"
---

# /reflection:review — Analyze Project Memory

> Part of the `/reflection:*` family (see `/reflection:promote`, `:extract`, `:remember`,
> `:consolidate`). This command runs a **read-only** audit of Claude Code's native project
> memory (and, when relevant, the session memory bank) and surfaces what to promote, prune,
> or merge. It changes nothing — acting on findings is `/reflection:promote` and
> `/reflection:consolidate`'s job.

## Usage

```
/reflection:review             # Full review (dashboard + candidates + stale + consolidation)
/reflection:review --brief      # One-line health summary only (no agent dispatch)
/reflection:review --quick      # Health dashboard + top 3 promotion candidates
/reflection:review --stale      # Focus on stale / outdated entries
/reflection:review --candidates # Focus on promotion candidates only
```

## `--brief` / `--quick` (no dispatch)

Run directly, no subagent:

```bash
claude-code memory doctor
```

Report: bank cap usage (activeContext/progress/decisionLog/conventions/sessionHistory vs
their caps), any stale findings, and (for `--quick`) a quick scan of
`~/.claude/projects/*/memory/MEMORY.md` line counts flagged if any are near the 200-line
auto-load cap.

## Full review (default, `--stale`, `--candidates`)

Dispatch the **opencode** agent in **ANALYZE** mode. Give it:

- Every `~/.claude/projects/*/memory/MEMORY.md` + linked topic files
- The output of `claude-code memory doctor`
- `rules/standards/*.md`, `rules/workflow/*.md`, and this project's `CLAUDE.md`

It returns the Memory Health Report (inventory, promotion candidates scored 0-6 on
Durability+Impact+Scope, stale entries, consolidation groups, conflicts). Filter the report
to the requested focus (`--stale` → Stale Entries section only; `--candidates` →
Promotion Candidates only); default shows everything.

## Report

Present the agent's report verbatim, followed by a one-line next-step suggestion per
finding category (e.g. "3 candidates ≥ score 6 → run `/reflection:promote` on each").
