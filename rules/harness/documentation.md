---
paths:
  - "**/*.md"
  - "**/*.markdown"
---
# Documentation Standards

How harness Markdown is written and where it lands. Placement is enforced by `docs-placement`
(`internal/hooks/docs/docsplacement.go`, `plan.go`).

## Writing

- **Living docs** — README, CLAUDE.md and AGENTS.md change in the same commit as the behavior they describe. Write for
  a reader without your context: outcome first, detail after; one concern per file.
- **CLAUDE.md is an index, ≤ ~120 lines** — it enters every session's prompt; point at `rules/`, skills and
  references, never copy a rule body into it. AGENTS.md is the cross-agent equivalent; if generated from `CLAUDE.md`,
  edit the source.
- **Formatting** — rules: `.markdownlint.json` is the rule set (VS Code and markdownlint-cli2);
  `.markdownlint-cli2.jsonc` holds the ignores; `.mdsmith.yml` is the gate twin and `mdlint.Parity` fails
  `make lint-docs` on drift. Go writers wrap at `mdlint.Width` (120 columns; tables and code blocks exempt): one H1,
  no skipped heading levels, `-` bullets, ATX headings, fenced code with a language. When: the Stop `format-stop`
  hook runs `mdsmith fix` on session-edited `.md`, `plans/` and `memory/` included; `review simplify`
  (review-simplicity hook) reports mdsmith findings on the Markdown lines an edit wrote (advisory). Gates:
  `claude-code agents diff --check --gates docs --path <files>` (mdsmith on changed `.md`, plus `mdlint.Parity` when
  a lint config changed or with `--all`); `make lint-docs CHECK=1` also runs markdownlint-cli2 (part of
  `make audit`). Fix: `claude-code agents fix --path <files>` for yours; `rtk make format` is main session only.
- **Naming and links** — lowercase-with-hyphens files, except the root anchors (the set under What passes),
  `SKILL.md`, `agents/Plan.md` and the memory bank file names (`activeContext.md`); relative intra-repo links. A moved
  or renamed file means fixing every doc that points at it in the same commit.

## Placement

Harness Markdown goes to the homes below, never a repo root or gitignored `docs/`.

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

### What passes

- Root anchors: `README.md`, `CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, `LICENSE.md`, `NOTICE.md`, `CHANGELOG.md`,
  `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `CLAUDE.local.md`, `AGENTS.override.md`;
  `.markdown` equivalents too. This repo keeps no root AGENTS/GEMINI file; `agents sync` renders to agent homes.
- Tracked docs and new pages in a trackable `docs/`; this repo's `docs/` holds only `images/` and `templates/`.
- Any tracked or existing file, other subdirectory or non-repo path. A new untracked/ignored root name still blocks.

Exception: a new root/docs `.md` with whole-word `plan`/`plans` in its name redirects to the dated plan home,
whether tracked or ignored.

### The Plan agent

The `Plan` subagent's Write/Edit/Bash writes pass only under `~/.claude/plans/`.
The leaf allow-and-note exception never lifts this block.

### When the gate blocks

Write to the named home; plan/report/handoff names get an exact dated path.
For a genuine repo doc the user requested, `touch <file> && git add -N <file>` (`-f` if ignored), then retry, or
`HARNESS_HOOK_SKIP=docs-placement` for the launch.
