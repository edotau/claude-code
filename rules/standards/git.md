# Git Workflow Standards

1. **Conventional Commits** — `<type>[scope][!]: <description>`; types `feat` `fix` `docs` `style` `refactor` `perf`
   `test` `chore` `ci`; scope = package or area; `!` + a `BREAKING CHANGE:` body line marks a break (→ MAJOR;
   `feat` → MINOR, `fix` → PATCH); `Fixes #42` where an issue exists. **1–2 lines total** — a body line only for
   what the diff cannot show, never a change inventory.
2. **Branches** — `feature/<domain>-<component>`, `fix/<issue>-<description>`, `docs/<component>`,
   `hotfix/<issue>-<description>`, `test/<feature>`; delete after merging.
3. **Stage named files only — never `git add -A` or a directory** (sweeps `.env`/secrets and other sessions'
   work); confirm with the user before committing.
4. Atomic commits, one logical change each; never force-push a shared branch; never skip gates.

```bash
feat(router): fail over on 529 before the first byte
fix(providers): expand ~ in auth.file
feat(providers)!: rename auth.cmd to auth.command   # + BREAKING CHANGE: body line
```

Executor mechanics (account switching, the `git rm` staging gotcha): `agents/copilot.md` → LAST-MILE.
