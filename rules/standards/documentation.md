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
  If a repo generates it from `CLAUDE.md`, edit the source, not the generated file.

## Markdown Formatting

- **Headings** — one H1 per file (the title); `##` for major sections, `###` for
  subsections. Don't skip levels.
- **Lists** — `-` for unordered, `1.` for ordered, `- [ ]` for task lists.
- **Code blocks** — always fence with a language tag (` ```bash `, ` ```python `,
  ` ```yaml `) for syntax highlighting.
- **Links** — relative paths for intra-repo links (`../standards/quality.md`); verify
  they resolve. No 404s, no broken references.
- **Tables** — use for comparisons and reference matrices; keep cells terse.

## File Naming

- Markdown docs: lowercase-with-hyphens (`git-workflow.md`), except the conventional
  uppercase anchors `README.md`, `CLAUDE.md`, `AGENTS.md`.
- Skill folders: lowercase, hyphenated (`code-review/`).

## Link Validation

Before committing docs: every relative link resolves, every referenced file exists, no
dangling cross-references. A moved/renamed file means updating every doc that points at
it in the same commit.
