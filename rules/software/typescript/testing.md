---
paths:
  - "**/*.ts"
  - "**/*.tsx"
  - "**/package.json"
  - "**/vitest.config.*"
---
# TypeScript Testing

> This file extends [standards/testing-guidelines.md](../../standards/testing-guidelines.md)
> with TypeScript/npm specific content.

- Scope every run to what changed (full suite: `quality.md` step 6): `npx tsc --noEmit -p <tsconfig>` (a single
  file cannot be typechecked alone), `npx eslint <files>`, `npx vitest run <path> [-t "<name>"]`.
- E2E (cypress/playwright) builds and serves the whole app: wrap-up only, user-approved.
- Read `package.json` `scripts` first and use the repo's runner; never `npm install` a test tool to run one.
- Mock at the network boundary (MSW or the RTK Query endpoint), not the hook under test; reset handlers per test.
- Generated clients (`rtk-query-codegen`, OpenAPI) are regenerated, never hand-edited.
- A snapshot is not a guard: prefer explicit assertions; a snapshot needs a reason in the test name.
