---
paths:
  - "**/*.md"
  - "**/*.markdown"
---
# Docs Placement — Harness Markdown Never Lands at a Repo Root

**Harness-authored markdown has three homes; a repo root and a gitignored `docs/` are off-limits.** The
`docs-placement` gate enforces it (`internal/hooks/docsplacement.go`; plan filing in `internal/hooks/plan.go`).

| Artifact | Path |
| --- | --- |
| Plan | `~/.claude/plans/YYYY-MM-DD-<slug>.md` (this repo) · `~/.claude/plans/<repo>/YYYY-MM-DD-<slug>.md` (others); gitignored; dated from the first write, mirrored by `docs-plan-mirror`, filed on approval by `docs-plan-save` |
| Session handoff | `<store>/handoffs/YYYY-MM-DD-<topic>.md` — only when `activeContext.md` can't hold it; point to it from there |
| Report (review, audit, techdebt scan, research brief) | `<store>/reports/YYYY-MM-DD-<name>.md` |
| Promoted knowledge (local-only) | `<store>/knowledge/<topic>.md` — home bank for cross-repo, the repo's own bank otherwise |

`<store>` is `~/.claude/memory/<repo>/` (`claude-code memory path`; the harness is `memory/claude-code/`, home is
`memory/home/`): a local-only git repo outside every project, so handoffs and reports never ride a project's
commits. The undated native plan file is a scratch draft; the dated copy is the file of record.

## What passes

- **Root anchors** — `README.md`, `CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, `LICENSE.md`, `NOTICE.md`, `CHANGELOG.md`,
  `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`. This repo keeps no `AGENTS.md`/`GEMINI.md` at its root:
  Claude Code would load it as a second CLAUDE.md; `claude-code agents sync` renders those into each agent's home.
- **A repo's own `docs/`** — tracked pages, and new pages in a `docs/` git would track. A **gitignored** `docs/`
  blocks: the page would vanish on a fresh clone. This repo's `docs/` takes only `images/` and `templates/`;
  any other new page there blocks.
- Any git-tracked file, any other subdirectory, non-repo paths.

## When the gate blocks

It names the canonical home; re-issue the write there. For a genuine repo doc the user asked for: `git add -N <file>`
first (tracked beats ignored), or disable the gate for the launch with `HARNESS_DOCS_PLACEMENT=0`.
