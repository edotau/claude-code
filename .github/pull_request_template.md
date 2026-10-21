## What and why

<!-- 1–3 sentences. Link the issue: Fixes #123 -->

## Type

- [ ] fix · [ ] feat · [ ] refactor · [ ] docs · [ ] test · [ ] chore/ci

## How it was checked

<!-- The live check (the verb/hook/flow run on real input) and the scoped tests, with their output. -->

## Checklist

- [ ] Title is a conventional commit (`feat(router): …`); breaking changes marked `!` with a `BREAKING CHANGE:` line
- [ ] `make audit` passes locally (CI runs the same gate on ubuntu + macos)
- [ ] Generated files regenerated: `settings.json` changes went through `internal/settings/template/`, not the
      rendered file; `make help-svg` / example goldens updated if menus or templates changed
- [ ] New behavior has a test that fails without the change
- [ ] Menu rows, help lines and flag text fit 80 columns
- [ ] No secrets, tokens or personal paths in code, fixtures, docs or logs
- [ ] Docs updated (README, CLAUDE.md index, rules/skills) when a command, verb or behavior changed
