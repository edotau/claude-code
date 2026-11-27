---
paths:
  - "**/*.md"
  - "**/*.markdown"
---
# Docs Placement — Harness Markdown Never Lands at a Repo Root

Harness Markdown goes to the homes below, never a repo root or gitignored `docs/`.
Enforced by `docs-placement` (`internal/hooks/docs/docsplacement.go`, `plan.go`).

- **Plan:** `~/.claude/plans/<repo>/YYYY-MM-DD-<slug>.md` (gitignored). Repo is the origin name, else main-checkout
  folder; use `<owner>-<repo>` for a remote-name collision and `@home` outside repos.
  `docs-plan` dates the first write and files approval. Undated plan-mode drafts are scratch.
- **Session handoff:** `~/.claude/memory/handoffs/YYYY-MM-DD-<topic>.md`, only when activeContext cannot hold it;
  link it from there.
- **Report:** `~/.claude/memory/reports/YYYY-MM-DD-<name>.md` (review, audit, research or debt scan).
- **Techdebt:** `~/.claude/memory/techdebt/<area>.md`, one per area; plans/reports may list it.
- **Promoted knowledge:** `~/.claude/memory/knowledge/<topic>.md`.

One local-only bank serves every workspace; reports and handoffs never enter project commits.
`claude-code memory path reports <name>` prints the exact path.

## What passes

- Root anchors: `README.md`, `CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, `LICENSE.md`, `NOTICE.md`, `CHANGELOG.md`,
  `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `CLAUDE.local.md`, `AGENTS.override.md`;
  `.markdown` equivalents too. This repo keeps no root AGENTS/GEMINI file; `agents sync` renders to agent homes.
- Tracked docs and new pages in a trackable `docs/`; this repo's `docs/` holds only `images/` and `templates/`.
- Any tracked or existing file, other subdirectory or non-repo path. A new untracked/ignored root name still blocks.

Exception: a new root/docs `.md` with whole-word `plan`/`plans` in its name redirects to the dated plan home,
whether tracked or ignored.

## The Plan agent

The `Plan` subagent's Write/Edit/Bash writes pass only under `~/.claude/plans/`.
The leaf allow-and-note exception never lifts this block.

## When the gate blocks

Write to the named home; plan/report/handoff names get an exact dated path.
For a genuine repo doc the user requested, `touch <file> && git add -N <file>` (`-f` if ignored), then retry, or
`HARNESS_HOOK_SKIP=docs-placement` for the launch.
