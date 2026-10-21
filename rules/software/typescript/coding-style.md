---
paths:
  - "**/*.ts"
  - "**/*.tsx"
---
# TypeScript Coding Style

> This file extends [standards/quality.md](../../standards/quality.md) with TypeScript specific content.

- `strict` stays on. No `any` (use `unknown` and narrow) and no non-null `!` without a comment proving it; no
  `@ts-ignore` — `@ts-expect-error` with a reason, if anything.
- Validate external data at the boundary (API responses, `localStorage`, URL params) with the repo's schema library
  (zod or similar) or a type guard; never `as SomeType` an unparsed response.
- Prefer `type` unions and `readonly` data; return new objects/arrays (spread, `map`, `toSorted`) — never mutate props
  or Redux state outside a reducer's Immer draft.
- React: function components and hooks; derive values instead of mirroring props into state; effects only for
  syncing with something outside React, each with complete dependencies (the lint rule wins; don't disable it).
- Imports: no default-export barrels that hide cycles; follow the repo's alias (`@/…`) and ESLint import order.
- Formatting is the repo's Prettier + ESLint config — run them, don't hand-format; `--max-warnings 0` means a warning
  is a failure.
