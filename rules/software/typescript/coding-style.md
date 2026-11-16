---
paths:
  - "**/*.ts"
  - "**/*.tsx"
---
# TypeScript Coding Style

> This file extends [standards/quality.md](../../standards/quality.md) with TypeScript specific content.

- `strict` stays on. No `any` (use `unknown` and narrow), no non-null `!` without a comment proving it, no
  `@ts-ignore` (`@ts-expect-error` with a reason, if anything).
- Validate external data (API responses, `localStorage`, URL params) at the boundary with the repo's schema library
  or a type guard; never `as SomeType` an unparsed response.
- Prefer `type` unions and `readonly` data; return new objects/arrays, never mutate props or Redux state outside a
  reducer's Immer draft.
- React effects only sync with something outside React, with complete dependencies (the lint rule wins).
- No default-export barrels that hide cycles; follow the repo's alias (`@/…`) and import order.
- Prettier and ESLint are the repo's config; `--max-warnings 0` makes a warning a failure.
