---
paths:
  - "**/*.md"
  - "**/*.markdown"
---
# Docs Placement — Harness Markdown Never Lands at a Repo Root

**Harness-authored markdown has three homes; a repo root and a gitignored `docs/` are off-limits.** The
`docs-placement` gate enforces it (`internal/hooks/docs/docsplacement.go`; plan filing:
`internal/hooks/docs/plan.go`).

| Artifact | Path |
| --- | --- |
| Plan | `~/.claude/plans/YYYY-MM-DD-<slug>.md` (this repo) · `~/.claude/plans/<repo>/YYYY-MM-DD-<slug>.md` (others); gitignored; dated from the first write (`docs-plan-mirror`), filed on approval (`docs-plan-save`); the undated draft is scratch |
| Session handoff | `<store>/handoffs/YYYY-MM-DD-<topic>.md` — only when `activeContext.md` can't hold it; point to it from there |
| Report (review, audit, techdebt scan, research brief) | `<store>/reports/YYYY-MM-DD-<name>.md` |
| Promoted knowledge (local-only) | `<store>/knowledge/<topic>.md` — home bank for cross-repo, the repo's own bank otherwise |

`<store>` is `~/.claude/memory/<repo>/` (`claude-code memory path`; harness `memory/claude-code/`, home `memory/home/`):
a local-only git repo outside every project, so handoffs and reports never ride a project's commits.

## What passes

- **Root anchors** — `README.md`, `CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, `LICENSE.md`, `NOTICE.md`, `CHANGELOG.md`,
  `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`. This repo keeps no root `AGENTS.md`/`GEMINI.md` (Claude Code
  would load it as a second CLAUDE.md); `claude-code agents sync` renders them into each agent's home.
- **A repo's own `docs/`** — tracked pages, and new pages in a `docs/` git would track. A gitignored `docs/` blocks
  (the page would vanish on a fresh clone); this repo's `docs/` takes only `images/` and `templates/`.
- Any git-tracked file, any other subdirectory, non-repo paths, and an existing git-ignored file (the owner's local
  doc; a new file under an ignored name still blocks).

## When the gate blocks

It names the canonical home; re-issue the write there. For a genuine repo doc the user asked for: `git add -N <file>`
first (tracked beats ignored), or `HARNESS_DOCS_PLACEMENT=0` for the launch.
