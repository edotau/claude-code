---
description: "Manual intervention (capture is normally automatic) — explicitly save a fact to native project memory as a structured topic file plus a MEMORY.md index line."
argument-hint: "<what to remember> [--type user|feedback|project|reference]"
---

# /reflection:remember — Save Knowledge Explicitly

> Part of the `/reflection:*` family (see `/reflection:review`, `:promote`, `:extract`,
> `:consolidate`). Use when a discovery is too important to rely on auto-capture, but is a
> *note*, not yet a *rule*. For rules use `/reflection:promote`; for reusable procedures use
> `/reflection:extract`.

## The schema (one file per fact + an index line)

Native project memory lives in the store dir printed by `claude-code memory path`
(`~/.claude/memory/<repo>/`), the same dir the six typed bank files live in. It is **structured**:
each fact is its own topic file with YAML
frontmatter, and `MEMORY.md` is a one-line-per-fact index. Do **not** just append a line to
`MEMORY.md` — write the topic file first, then add its index pointer.

Topic file frontmatter:

```markdown
---
name: <slug>              # kebab-case, matches the filename (no .md)
description: "<one-line, self-contained summary>"
metadata:
  type: project           # user | feedback | project | reference
---

<body: 1-3 short paragraphs — the fact, why it matters, how to apply.>
```

`metadata.type` values:

| type | Use for |
|------|---------|
| `project` | Project convention, architecture, gotcha (default) |
| `user` | A stable user preference ("prefers explicit error handling") |
| `feedback` | A correction the user made that should persist |
| `reference` | A durable external/pointer fact (endpoint, provider name) |

## Step 1: Parse the fact

Extract the concrete fact, why it matters, and scope. Choose a kebab-case `<slug>` and the
`metadata.type` (honor `--type`; default `project`).

## Step 2: Resolve dir and check for duplicates

```bash
MEMORY_DIR="$(claude-code memory path)"
grep -rni "<keywords>" "$MEMORY_DIR"/
```

If a near-duplicate topic file exists, show it and ask: update the existing file, or add a
new one? Never silently create a second file for the same fact.

## Step 3: Write the topic file

Write `"$MEMORY_DIR/<slug>.md"` using the schema above. Keep the body tight — a fact, not
an essay. Never store secrets, tokens, or credentials.

## Step 4: Add the index line to MEMORY.md

Append one line to `MEMORY.md`:

```markdown
- [<Title Case Name>](<slug>.md) — <concise description>
```

Match the existing index style exactly (the em-dash separator, lowercase `.md` link).

## Step 5: Suggest the next tier

If the fact reads like an imperative rule (always/never, a convention), nudge:

```
This sounds like a rule. Consider /reflection:promote to graduate it to a rules/ file
for durable enforcement.
```

## Step 6: Report

```
Saved to native project memory:
  <slug>.md      (type=<type>)
  MEMORY.md      +1 index line
MEMORY.md: N index lines
Claude will see this at the start of every session in this project.
```

## Don't use for

- Temporary context — just say it in conversation.
- Enforced rules — use `/reflection:promote`.
- Reusable procedures — use `/reflection:extract`.
- Secrets — never.
