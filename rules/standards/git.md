# Git Workflow Standards

## Core Principles

1. **Conventional Commits** — every commit follows the spec below; breaking changes marked
   explicitly (`!` after type/scope + a `BREAKING CHANGE:` body line).
2. **Semantic Versioning** — automated from commit types: BREAKING CHANGE → MAJOR,
   `feat` → MINOR, `fix` → PATCH.
3. **Branch Protection** — main requires review and green quality gates; no force pushes to
   shared branches.

## Commit Message Format

```
<type>[optional scope][!]: <description>

[optional body]
```

Types: `feat` `fix` `docs` `style` `refactor` `perf` `test` `chore` `ci`.
Scope is the package, module, or area touched (`router`, `providers`, `skills`, `docs`).

```bash
feat(router): fail over on 529 before the first byte
fix(providers): expand ~ in auth.file
feat(providers)!: rename auth.cmd to auth.command   # + BREAKING CHANGE: body line
```

Reference issue numbers in the body (`Fixes #42`) where one exists.

**Length: 1–2 lines total, strictly.** The subject usually IS the whole message; add a body line only
when it carries something the diff cannot show. No bullet lists, no change inventories, no summaries of
what the diff already says.

## Branch Naming

```
feature/agents-<name>   feature/skills-<name>   feature/<domain>-<component>
docs/<component>        fix/<issue>-<description>
hotfix/<issue>-<description>   test/<feature>
```

## Do / Don't

**Do**

- ✅ Conventional commit format for all commits; reference issues
- ✅ Atomic commits (one logical change each) with descriptive messages
- ✅ Test before committing; pull before pushing
- ✅ Feature branches for all changes; delete branches after merging

**Don't**

- ❌ Commit directly to a shared main — use PRs (a solo repo may say otherwise in its CLAUDE.md)
- ❌ Force push to shared branches
- ❌ Commit secrets or credentials
- ❌ AI-attribution footers (`Co-Authored-By: Claude`, `Generated with …`) — the committer identity covers it
- ❌ Mix unrelated changes in one commit
- ❌ Vague messages ("fix stuff", "updates"); skipped quality checks
- ❌ Leave branches unmerged for extended periods

## Staging Gotcha

`git rm` stages the deletion immediately, and a later `git commit` (without `-a`) commits the whole
index -- so an unrelated deletion silently rides along in the next commit. Run `git status` and inspect
the staged set before composing atomic commits, not just the files you intend to add.
