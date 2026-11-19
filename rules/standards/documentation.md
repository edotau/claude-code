---
paths:
  - "**/*.md"
---

# Documentation Standards

- **Living docs** — README, CLAUDE.md and AGENTS.md change in the same commit as the behavior they describe. Write for
  a reader without your context: outcome first, detail after; one concern per file.
- **CLAUDE.md is an index, ≤ ~120 lines** — it enters every session's prompt; point at `rules/`, skills and
  references, never copy a rule body into it. AGENTS.md is the cross-agent equivalent; if generated from `CLAUDE.md`,
  edit the source. Where harness markdown may land: `rules/harness/docs-placement.md`.
- **Formatting** — `.markdownlint-cli2.jsonc` (mirrored by `.mdsmith.yml`) is the standard (`agents fix --path <files>`
  fixes yours, `rtk make fmt` is main session only; `agents diff --check --gates docs --path <files>` checks docs):
  120 columns, one H1, no skipped heading levels, `-` bullets, fenced code with a language.
- **Naming and links** — lowercase-with-hyphens files, except the root anchors (`README.md`, `CLAUDE.md`,
  `AGENTS.md`, the set in `rules/harness/docs-placement.md`), `SKILL.md`, `agents/Plan.md` and the memory bank file
  names (`activeContext.md`); relative intra-repo links. A moved or renamed file means fixing every doc that points at
  it in the same commit.
