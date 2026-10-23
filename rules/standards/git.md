# Git Workflow Standards

1. **Conventional Commits** — `<type>[scope][!]: <description>` plus an optional body. Types `feat` `fix` `docs`
   `style` `refactor` `perf` `test` `chore` `ci`; scope = the package or area; `!` plus a `BREAKING CHANGE:` body
   line marks a break; `Fixes #42` in the body where an issue exists. **1–2 lines total**: the subject is usually
   the whole message; a body line only for what the diff cannot show — never a change inventory.
2. **Semantic versioning** follows the types: BREAKING CHANGE → MAJOR, `feat` → MINOR, `fix` → PATCH.
3. **Branches**: `feature/<domain>-<component>`, `fix/<issue>-<description>`, `docs/<component>`,
   `hotfix/<issue>-<description>`, `test/<feature>`; main is protected (review + green gates, no force push);
   delete branches after merging.

```bash
feat(router): fail over on 529 before the first byte
fix(providers): expand ~ in auth.file
feat(providers)!: rename auth.cmd to auth.command   # + BREAKING CHANGE: body line
```

**Do**: atomic commits (one logical change each); test before committing; pull before pushing; feature branches
and PRs for shared mains (a solo repo may say otherwise in its CLAUDE.md).
**Don't**: force-push shared branches; commit secrets; mix unrelated changes; vague messages ("fix stuff");
skipped gates; AI-attribution footers (`Co-Authored-By: Claude`, `Generated with …`) — the committer identity
covers it.

**Stage named files only — never `git add -A`** (it sweeps `.env`/secrets); confirm with the user before
committing. Executor mechanics (account switching, the `git rm` staging gotcha): `agents/copilot.md` → LAST-MILE.
