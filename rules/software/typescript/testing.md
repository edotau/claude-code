---
paths:
  - "**/*.ts"
  - "**/*.tsx"
  - "**/package.json"
  - "**/vitest.config.*"
---
# TypeScript Testing

Extends [testing-guidelines.md](../../standards/testing-guidelines.md).

- Read `package.json` scripts and use the repo's runner; never install a test tool for one run.
- Scope checks: `npx tsc --noEmit -p <tsconfig>` (not one file), `npx eslint <files>`,
  `npx vitest run <path> [-t "<name>"]`. Full suite: `quality.md` → Verification Order.
- Cypress/Playwright E2E builds and serves the whole app: owner-approved wrap-up only.
- Mock the network boundary (MSW or RTK Query endpoint), not the tested hook; reset handlers per test.
- Regenerate clients (`rtk-query-codegen`, OpenAPI), never hand-edit.
- Prefer explicit assertions; snapshots need a reason in the test name.
