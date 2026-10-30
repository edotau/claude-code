---
paths:
  - "**/*.go"
---

# CLI and TUI Text

**Prefer one line of ≤80 characters, label column included.** A description may wrap only with a
hanging indent: every continuation line starts exactly at the description column, never under the label
column. Arrow-key TUI menu rows (`internal/configure`) stay single-line — wrapping breaks the highlight and
the row→key mapping. Be concise but descriptive — every word counts.

- Lead with what the item does; drop filler ("this will", "allows you to") and `·`-chained clauses.
- Name the one fact the user needs to choose (the key it writes, the command it runs), not every effect.
- One width test per menu table, driving the real renderer.
- `-h` text is only ever a `helptext.Page` (`helptext.NewFlagSet` + `helptext.Parse`); FLAGS rows come from the FlagSet.

```text
WRONG  Mint key      API Keys API · creates an API key in your Cloud project and prints its value
RIGHT  Mint key      create an API key in your Cloud project
RIGHT  Mint key      create an API key in your Cloud project and print its
                     value once (wrapped: continuation at the description column)
```
