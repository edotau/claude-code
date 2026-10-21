---
paths:
  - "**/*.ts"
  - "**/*.tsx"
  - "**/package.json"
  - "**/vitest.config.*"
---
# TypeScript Testing

> This file extends [standards/quality.md](../../standards/quality.md) with TypeScript/npm specific content.

## Scope every run to what changed

The full suite (`npm test`, `npm run test`, bare `npx vitest run`, `--coverage`, cypress/e2e) runs once, at wrap-up,
with the user's approval — the same rule as `make test`; an `ask` permission enforces it. During development and in
every subagent brief, name the files:

| Step | Command |
| --- | --- |
| Typecheck | `npx tsc --noEmit -p <tsconfig>` (the project's own; a single file can't be typechecked alone) |
| Lint | `npx eslint <changed files>` — the repo's `npm run lint` covers everything, use it at wrap-up |
| One test file | `npx vitest run <path/to/file.test.tsx>` (jest: `npx jest <path>`) |
| One test | `npx vitest run <path> -t "<name>"` |
| Tests for a dir | `npx vitest run src/<feature>/` |
| Coverage of a dir | `npx vitest run src/<feature>/ --coverage --coverage.include='src/<feature>/**'` |
| E2E (cypress/playwright) | wrap-up only, user-approved: it builds and serves the whole app |

Read `package.json` `scripts` first and use the repo's runner and flags; never `npm install` a test tool to run one.

## Writing tests

- Vitest + Testing Library: query by role/label/text (`getByRole`), never by class or test-id first; assert what the
  user sees, not component internals.
- Mock at the network boundary (MSW or the RTK Query endpoint), not the hook under test; reset handlers per test.
- Use `userEvent` over `fireEvent`; `await` every async query (`findBy…`), no fixed `setTimeout` waits.
- Generated clients (`rtk-query-codegen`, OpenAPI) are regenerated, never hand-edited; test against them, not inside.
- A snapshot is not a guard: prefer explicit assertions; a snapshot needs a reason in the test name.
