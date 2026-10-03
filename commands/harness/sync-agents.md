---
description: "Mirror agents/ + skills/ into Gemini CLI (~/.gemini/skills) and regenerate AGENTS.md/GEMINI.md"
argument-hint: "[--dry-run | --check | --no-prune]"
---

# /harness:sync-agents

Project `agents/` and real `skills/` into the Gemini CLI's native skill discovery
(`~/.gemini/skills/<name>/SKILL.md`, per-user under `$HOME`, not per-repo), and render
`AGENTS.md`/`GEMINI.md` at the config root from the same roster.

## What syncs

- **Every agent** in `agents/<name>.md` — Gemini has no agent concept, so each agent is
  projected as a description-matched skill (`name` + `description`, cleaned of `<example>` blocks).
- **Every real skill** in `skills/<name>/SKILL.md` whose directory is a real dir (not a symlink) with
  a regular `SKILL.md` — cited assets (`scripts/`, `references/`, `hooks/`, loose `.md`) are symlinked
  alongside the projection, so they stay current without a re-sync.

Each generated file carries a `<!-- Synced from ... -->` banner.

## What's skipped

- Names in `skipNames` (`internal/crossgen/transform.go`) — Claude-Code-harness-only agents/skills
  (e.g. `approval-gate`) that have no meaning outside this CLI.
- Symlinked skill directories under `skills/` (e.g. `skills/<x>` → `../../.agents/skills/<x>`) —
  Gemini already discovers `~/.agents/skills/*` natively, so projecting them would create duplicate
  names. This also excludes `skills/synced/`.
- Any hand-authored directory in `~/.gemini/skills` without the sync banner — never overwritten,
  never pruned.

## Pruning

Prune only ever removes a **banner-marked** generated dir whose source (agent or skill) no longer
exists or is now skip-listed — never a symlink entry, never a bannerless hand-made directory. Pass
`--no-prune` to skip this pass entirely.

## Execution

```bash
claude-code agents sync --gemini $ARGUMENTS
claude-code agents docs [--check|--dry-run from $ARGUMENTS]
gemini skills list
```

`agents sync --gemini` writes the projections (respects `--dry-run`/`--check`/`--no-prune` from
`$ARGUMENTS`). `agents docs` regenerates `AGENTS.md` and `GEMINI.md` at the config root from the same
roster — both are gitignored, generated output. `docs` takes only `--check`/`--dry-run`: when
`$ARGUMENTS` contains one of those, forward it verbatim (`claude-code agents docs --check`); drop
`--no-prune` from what is forwarded — `docs` has no such flag and would error on it. With neither flag
present, run `claude-code agents docs` bare. `gemini skills list` confirms the Gemini CLI sees the
projections with no duplicate names against `~/.agents/skills`.

Report the output.
