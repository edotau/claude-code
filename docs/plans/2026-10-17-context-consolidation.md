# Context consolidation: Orca worktrees × ~/.claude — 2026-10-17

Goal: one place per concern (memory, skills, instructions) so every agent in every worktree loads the same context once.

## Measured (per Claude Code request, before)

| Source | Items | ~tokens | Note |
| --- | --- | --- | --- |
| ~/.claude/AGENTS.md | 1 | ~3,800 | byte copy of CLAUDE.md, loaded as "project instructions" in every repo without its own CLAUDE.md — **removed** |
| agent descriptions (Agent tool list) | 9 | ~4,300 | `<example>` blocks inside `description:` — user decision |
| claude.ai synced skills | 35 | ~4,500 | scientific pack + dupes of evaluate-code, computer-use, frontend-design — prune on claude.ai |
| plugin skills (databricks 31, impeccable, frontend-design…) | 45 | ~4,200 | disable plugins not in use via /plugin |
| user skills (resume pack, orca) | 24 | ~900 | symlinks in ~/.claude/skills are REQUIRED: Claude Code does not read ~/.agents/skills (probed) |
| harness skills + commands | 31 | ~1,400 | |

## Done

- Instruction render moved off the harness root into the vendors' GLOBAL files: `~/.codex/AGENTS.md`, `~/.gemini/GEMINI.md`
  (tracked `dist/<vendor>/…`, banner-marked, hand-authored kept). Probe: a worktree session now loads CLAUDE.md + 5 rules only.
  Copilot in ~/.claude loaded AGENTS.md + GEMINI.md + CLAUDE.md (3 copies) → 1. `agents docs` verb folded into `agents sync`.
- Memory: linked worktrees share the main checkout's bank (`paths.MainCheckout`); the `main` and stale-clone `claude-code`
  banks were merged/parked. `~/.codex/AGENTS.md` Codex-import copy parked.
- `claude-code clean --what plugin-versions` sweeps plugin cache versions not named in installed_plugins.json (29 dirs, 4.8 MiB here).

## Orca findings (user decisions — Orca has no CLI verb to unregister a repo)

- Orca registers THREE harness repos: `/Users/edotau/claude-code` (path no longer exists), `/Users/edotau/orca/claude-code`
  (clone 32 commits behind origin, clean), and the live `~/.claude` is NOT registered. Agents launched in the stale clone edit a
  harness that is not live and write a separate memory bank. Recommend: remove both registrations in the Orca UI; work on the
  harness in `~/.claude` (the only live checkout) — do not register it as an Orca repo (an Orca worktree of it would not be live).
- Orca created the worktree `~/.claude/workspaces/edotau.github.io/anthias` (branch edotau/shell-cd-command, 229 MB) INSIDE the
  harness checkout; its other site worktree `~/orca/workspaces/edotau.github.io/main` is not Orca-managed. Recommend: point Orca's
  worktree root at `~/orca/workspaces`, `orca worktree rm` the anthias one when done (or recreate it there). `workspaces/` is
  gitignored and the bank slug now resolves to `edotau.github.io` either way.
- Orca's 13 hook entries only spool events to the Orca app (no context injection) — no duplication, just per-event shell spawns.

## Not changed (needs the user)

- Skill pruning on claude.ai (synced pack) and `/plugin` disables; trimming agent `<example>` blocks.
- OpenCode reads both ~/.claude/skills and ~/.agents/skills (log WARN per duplicate, last wins); the only switch also hides
  repo-local `.claude/skills`.
