---
paths:
  - "**/*.go"
---

# CLI and TUI Text

**Every menu row, help/usage line and flag description fits one line of ≤80 characters**, label
column included. Be concise but descriptive — every word counts.

- Lead with what the item does; drop filler ("this will", "allows you to") and `·`-chained clauses.
- Name the one fact the user needs to choose (the key it writes, the command it runs), not every effect.
- Data-driven menus carry a width test that renders each row and asserts ≤80.

```text
WRONG  Mint key      API Keys API · creates an API key in your Cloud project and prints its value
RIGHT  Mint key      create an API key in your Cloud project
```
