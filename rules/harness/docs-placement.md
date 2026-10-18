---
paths:
  - "**/*.md"
  - "**/*.markdown"
  - "internal/hooks/docsplacement.go"
---
# Docs Placement — Harness Markdown Lives Under `~/.claude/docs`, Never a Repo Root

> Canonical location rule for every markdown file the harness (or a session working through it)
> authors. Enforced by the `docs-placement` PreToolUse gate — see `guardrails.md`.

## Rule

**Harness-authored markdown goes under the harness docs tree; a repo root is off-limits.** The tree
is `~/.claude/docs`; `internal/hooks/docsplacement.go` is the only place its subpaths are spelled.

| Artifact | Path | Producer |
| --- | --- | --- |
| Approved plan | `~/.claude/plans/<date>-<title>.md` (this repo) · `~/.claude/plans/<repo>/<date>-<title>.md` (any other repo) — Claude Code's native `plansDirectory`, **outside** the docs tree and gitignored wholesale | `save-plan` hook (`ExitPlanMode`) |
| Session handoff | `~/.claude/memory/<repo>/handoffs/<date>-<topic>.md` | the session that ends, pointed to from `activeContext.md`; only when activeContext can't hold it |
| Command/agent report (review, audit, techdebt scan, research brief) | `docs/reports/<repo>/<name>.md` | the command or agent that produced it |
| Session memory bank (STATE, not a doc) | `~/.claude/memory/<repo>/` | `memory init` / the harvest gate |

The **repo tag** is `paths.RepoSlug(root)` — the lowercased repo basename. The harness checkout itself
resolves to `""`, so its own plans and handoffs sit untagged at the top of each subtree. Handoffs/reports
stay tracked under `docs/`; plans do not — `plans/` is gitignored wholesale, so even this repo's own
plans live only on disk, never in git history.

## What stays out of the docs tree

- **Root anchors** — `README.md`, `CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, `LICENSE.md`,
  `NOTICE.md`, `CHANGELOG.md`, `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`. A repo's own
  `AGENTS.md`/`GEMINI.md` belong at its root (Codex/Gemini/Copilot read them there). The harness itself keeps
  none at its root: `claude-code agents sync` renders CLAUDE.md + the roster into `~/.codex/AGENTS.md` and
  `~/.gemini/GEMINI.md` (rendered locally under `docs/projections/`), because Claude Code would load a root copy as a second
  CLAUDE.md in every session.
- **A repo's own documentation** — `docs/` pages a project owns. Tracked pages always pass, and a
  brand-new page in a `docs/` the repo *could* track passes too (it is merely not-yet-added). The
  exception is a **gitignored** `docs/`: git refuses to keep anything there, so a page written to it
  is invisible on every fresh clone — that is stranded harness output, and the gate blocks it.
- **State, not docs** — `~/.claude/memory/` (the unified memory store and its locks — its own local
  git repo, outside this repo's `docs/` tree entirely), `docs/projections/` (per-vendor agent
  renders from `agents sync`; `clean --target projections` sweeps them) and `state/harness/`.

## Enforcement — the `docs-placement` gate

`PreToolUse(Write|Edit)` → `claude-code hook docs-placement`. It **blocks** a `.md`/`.markdown` write
in either of two shapes, printing the canonical path to use instead:

1. the target directory *is* the git toplevel (a repo root); or
2. the target sits at or under `<root>/docs` **and** git ignores it — a page that vanishes on a fresh
   clone (`git check-ignore`; a non-ignored `docs/` is the project's own and is never gated).

It passes on: a root anchor, a git-**tracked** file (`git ls-files`, so a force-added page inside an
ignored `docs/` passes), anything at or under the harness docs tree itself, every other subdirectory,
a non-markdown path, a non-repo location, and anything it can't resolve (fail-open).

- Bank/handoff writes land in `~/.claude/memory/`, outside any repo's `docs/` tree, so this gate
  never sees them at all. Plans don't hit it either — `save-plan` writes directly to
  `~/.claude/plans/`, outside the docs tree and outside any repo's own `docs/`.
- Class: deny-one-call — not a Stop/SubagentStop gate, so it needs no fail-open counter. The model
  re-issues the write at the named path and moves on.
- A genuine repo doc the user asked for: `git add -N <file>` first (tracked wins over ignored), or
  disable the gate for the launch with `HARNESS_DOCS_PLACEMENT=0`.

## Related

- `guardrails.md` — the gate inventory this gate joins
- `hooks.md` — the event → hook matrix
- `../standards/documentation.md` — markdown quality; the gate's block message points here
