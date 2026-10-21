---
paths:
  - "**/*.md"
---

# Documentation Standards

## Core Principles

1. **Living documentation** — README, CLAUDE.md, and AGENTS.md stay current with the
   code. Update docs in the same change as the behavior they describe.
2. **Clarity & accessibility** — write for a reader without your context. Lead with the
   outcome; supporting detail after.
3. **Structure & consistency** — predictable headings, one concern per file.

## CLAUDE.md / AGENTS.md

- **Keep CLAUDE.md ≤ ~200 lines.** It is injected into every session's system prompt —
  every line costs context on every turn. Move detail to `rules/`, skills, or references.
- CLAUDE.md is the always-on tier; `rules/` is on-demand. Don't duplicate rule bodies
  into CLAUDE.md — point to them.
- `AGENTS.md` is the cross-agent equivalent (other agent CLIs read it from the project root).
  If a repo generates it from `CLAUDE.md`, edit the source, not the generated file. This repo does:
  `claude-code agents sync` renders `CLAUDE.md` + `agents/` into each vendor's own file
  (`~/.codex/AGENTS.md`, `~/.gemini/GEMINI.md`); local renders live under `state/projections/`.
  This repo keeps no `AGENTS.md` or `GEMINI.md` at its root.

## Markdown Formatting

The enforced standard is `.markdownlint-cli2.jsonc` (VS Code's markdownlint) mirrored by `.mdsmith.yml`
(`make lint`, which auto-fixes; `CHECK=1` only reports). 120-column lines, ATX headings, `-` bullets,
fenced code with a language, `*`/`**` emphasis.

- **Headings** — one H1 per file (the title); `##` for major sections, `###` for
  subsections. Don't skip levels.
- **Lists** — `-` for unordered, `1.` for ordered, `- [ ]` for task lists.
- **Code blocks** — always fence with a language tag (` ```bash `, ` ```python `,
  ` ```yaml `) for syntax highlighting.
- **Links** — relative paths for intra-repo links (`../standards/quality.md`); verify
  they resolve. No 404s, no broken references.
- **Tables** — use for comparisons and reference matrices; keep cells terse.

## Document Types

| Type | Must cover |
|---|---|
| README | What it is and why; quick start (<5 min to first success); configuration and usage; contributing |
| API | Endpoint reference with request/response examples; auth and error codes; rate limits, pagination |
| Runbook | When to use it; prerequisites and access; step-by-step procedure; rollback; escalation path |
| Architecture | Context and goals; high-level design with a diagram; key decisions and trade-offs; data flow |
| Onboarding | Environment setup; key systems and how they connect; common tasks walked through; who to ask |

Adapted from anthropics/knowledge-work-plugins (engineering), Apache-2.0.

## File Naming

- Markdown docs: lowercase-with-hyphens (`git-workflow.md`), except the conventional
  uppercase anchors `README.md`, `CLAUDE.md`, `AGENTS.md`.
- Skill folders: lowercase, hyphenated (`code-review/`).

## Link Validation

Before committing docs: every relative link resolves, every referenced file exists, no
dangling cross-references. A moved/renamed file means updating every doc that points at
it in the same commit.
