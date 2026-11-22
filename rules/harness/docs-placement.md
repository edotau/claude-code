---
paths:
  - "**/*.md"
  - "**/*.markdown"
---
# Docs Placement — Harness Markdown Never Lands at a Repo Root

**Harness-authored markdown has four homes; a repo root and a gitignored `docs/` are off-limits.** The
`docs-placement` gate enforces it (`internal/hooks/docs/docsplacement.go`; plan filing:
`internal/hooks/docs/plan.go`).

| Artifact | Path |
| --- | --- |
| Plan | `~/.claude/plans/YYYY-MM-DD-<slug>.md` (this repo) · `~/.claude/plans/<repo>/YYYY-MM-DD-<slug>.md` (others); gitignored; dated from the first write (`docs-plan-mirror`), filed on approval (`docs-plan-save`); the undated draft is scratch |
| Session handoff | `~/.claude/memory/handoffs/YYYY-MM-DD-<topic>.md` — only when `activeContext.md` can't hold it; point to it from there |
| Report (review, audit, techdebt scan, research brief) | `~/.claude/memory/reports/YYYY-MM-DD-<name>.md` |
| Techdebt | `~/.claude/memory/techdebt/<area>.md`, one file per area; a plan or report may list it, its home is here |
| Promoted knowledge (local-only) | `~/.claude/memory/knowledge/<topic>.md` |

The store is one bank for every workspace; `claude-code memory path reports <name>` prints the exact path. It is a
local-only git repo outside every project, so handoffs and reports never ride a project's commits.

## What passes

- **Root anchors** — `README.md`, `CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, `LICENSE.md`, `NOTICE.md`, `CHANGELOG.md`,
  `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `CLAUDE.local.md`, `AGENTS.override.md`, and any of them
  spelled `.markdown`. This repo keeps no root `AGENTS.md`/`GEMINI.md` (Claude Code would load it as a second
  CLAUDE.md); `claude-code agents sync` renders them into each agent's home.
- **A repo's own `docs/`** — tracked pages, and new pages in a `docs/` git would track. A gitignored `docs/` blocks
  (the page would vanish on a fresh clone); this repo's `docs/` takes only `images/` and `templates/`.
- Any git-tracked file, any other subdirectory, non-repo paths, and any existing file (the owner's local doc; a new
  untracked or ignored name still blocks).

One exception inside a repo: a new plan-named `.md` (`plan`/`plans` as a whole word) at the root or in `docs/`,
tracked or ignored, is redirected to the dated plans home (`docs-placement`).

## The Plan agent

A `Write`/`Edit` from the `Plan` subagent (payload `agent_type`) passes only under `~/.claude/plans/`; any other path
blocks. Its Bash passes only `permission.ReadOnlyShell` (a `safety-scan` floor block no `--allow` lifts).

## When the gate blocks

It names the canonical home; re-issue the write there. When the file name holds plan, report or handoff, it names the
one exact dated path to write. For a genuine repo doc the user asked for:
`touch <file> && git add -N <file>` (`-f` if ignored) first, then re-issue the write, or
`HARNESS_HOOK_SKIP=docs-placement` for the launch.
