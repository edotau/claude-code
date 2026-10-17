---
paths:
  - "rules/**"
  - "commands/harness/adapt.md"
---
# Standards — Coding Doctrine Tier

The **writable, portable** rules tier: how code should be written, reviewed, committed,
documented, and secured.

## What lives here

| File | Covers |
|------|--------|
| `communication.md` | Honesty, zero-fluff, file economy, anti-overengineering, response protocol |
| `quality.md` | Code quality, immutability, testing (live check first, scoped tests, the suite once), zero-defect completion |
| `git.md` | Conventional commits, branch naming, do/don't guardrails |
| `documentation.md` | Markdown quality, ≤200-line CLAUDE.md, living docs, link validation |
| `security.md` | Secret detection, input validation, dependency + data protection |
| `performance.md` | Model selection, context-window management, extended thinking |

## How this tier is used

- **Auto-injected every turn** — the six doctrine files here are unscoped (this README is not), so
  Claude Code loads them into every session prompt. Adding `paths:` frontmatter is what makes one
  on-demand; keep each file short, since every line costs context on every turn.
- **`/harness:adapt` writes here** — session corrections are routed into the matching file (see the
  routing table in `commands/harness/adapt.md`). Keep entries thin: imperative voice, a line to a
  short paragraph.
- **Promotion** — a rule that keeps recurring across agent memories and sessions is promoted by
  `/reflection:consolidate`; `CLAUDE.md` stays an index and takes only hard rules.
- **Cross-agent bridge** — this tier reaches other tools only via `CLAUDE.md` prose, which
  `claude-code agents sync` renders into `~/.codex/AGENTS.md`/`~/.gemini/GEMINI.md`, projects agents into every vendor home via tracked `dist/`, and links skills into `~/.agents/skills`. `workflow/` and `harness/` are not bridged at all.

## Priority order (when standards conflict)

1. **Quality** — zero-defect handoff
2. **Git** — conventional commits for all changes
3. **Documentation** — living docs stay current
4. **Communication** — clear, pragmatic, actionable
5. **Security** — non-negotiable, always enforced

## Language overlays

`rules/software/<lang>/<topic>.md` extends a standard with language-specific content
(e.g. `software/python/testing.md` → `> extends ../../standards/quality.md`). Language rules
override the general standard where idioms differ.
