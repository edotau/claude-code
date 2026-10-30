---
paths:
  - "**/*.md"
---

# Documentation Standards

- **Living docs** — README, CLAUDE.md and AGENTS.md change in the same commit as the behavior they describe.
  Write for a reader without your context: outcome first, detail after; one concern per file.
- **CLAUDE.md is an index, ≤ ~200 lines** — it enters every session's prompt. Point at `rules/`, skills and
  references; never copy a rule body into it.
- **AGENTS.md** is the cross-agent equivalent at a project root. If a repo generates it from `CLAUDE.md`, edit the
  source, not the output. Where harness markdown may land: `rules/harness/docs-placement.md`.
- **Formatting** — `.markdownlint-cli2.jsonc`, mirrored by `.mdsmith.yml`, is the enforced standard (`make lint`
  auto-fixes; `CHECK=1` only reports): 120 columns, one H1, no skipped heading levels, `-` bullets, fenced code
  with a language.
- **Naming** — lowercase-with-hyphens (`git-workflow.md`, `code-review/`), except the uppercase root anchors
  (`README.md`, `CLAUDE.md`, `AGENTS.md`).
- **Links** — relative paths for intra-repo links. Before committing, every link resolves; a moved or renamed file
  means updating every doc that points at it in the same commit.
