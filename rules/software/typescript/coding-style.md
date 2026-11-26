---
paths:
  - "**/*.ts"
  - "**/*.tsx"
---
# TypeScript Coding Style

Extends [quality.md](../../standards/quality.md).

- Keep `strict`; use `unknown` and narrow, never `any`. Non-null `!` needs a proof comment.
  No `@ts-ignore`; any `@ts-expect-error` needs a reason and the suppression exception in `quality.md`.
- Validate API, localStorage and URL input with the repo's schema library or a type guard; never cast unparsed data.
- Prefer type unions and readonly data; return new objects/arrays. Mutate Redux state only in a reducer's Immer draft.
- React effects synchronize with external systems; keep all dependencies the lint rule requires.
- No default-export barrels hiding cycles; follow repo aliases and import order.
- Use repo Prettier/ESLint config; `--max-warnings 0` makes warnings fail.
