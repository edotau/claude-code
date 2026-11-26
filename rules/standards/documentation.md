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
- **Formatting** — rules: `.markdownlint.json` is the rule set (VS Code and markdownlint-cli2);
  `.markdownlint-cli2.jsonc` holds the ignores; `.mdsmith.yml` is the gate twin and `mdlint.Parity` fails
  `make lint-docs` on drift. Go writers wrap at `mdlint.Width` (120 columns; tables and code blocks exempt): one H1,
  no skipped heading levels, `-` bullets, ATX headings, fenced code with a language. When: the Stop `format-stop`
  hook runs `mdsmith fix` on session-edited `.md`, `plans/` and `memory/` included; `review simplify`
  (review-simplicity hook) reports mdsmith findings on the Markdown lines an edit wrote (advisory). Gates:
  `claude-code agents diff --check --gates docs --path <files>` (mdsmith on changed `.md`, plus `mdlint.Parity` when
  a lint config changed or with `--all`); `make lint-docs CHECK=1` also runs markdownlint-cli2 (part of
  `make audit`). Fix: `claude-code agents fix --path <files>` for yours; `rtk make fmt` is main session only.
- **Naming and links** — lowercase-with-hyphens files, except the root anchors (`README.md`, `CLAUDE.md`,
  `AGENTS.md`, the set in `rules/harness/docs-placement.md`), `SKILL.md`, `agents/Plan.md` and the memory bank file
  names (`activeContext.md`); relative intra-repo links. A moved or renamed file means fixing every doc that points at
  it in the same commit.
