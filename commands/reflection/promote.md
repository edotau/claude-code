---
description: "Graduate a proven memory entry into a writable rules tier (rules/standards or rules/software) — never CLAUDE.md directly — then remove it from native project memory."
argument-hint: "<pattern description> [--target <path>]"
---

# /reflection:promote — Graduate a Learning up the Gradient

> Part of the `/reflection:*` family (see `/reflection:review`, `:extract`, `:remember`,
> `:consolidate`). This command moves a proven entry **out of** native project memory and
> **into** a writable rules tier where it becomes durable, addressable doctrine.

## The promotion gradient

```
memory entry ──promote──▶ rules/standards/<topic>.md
                                 rules/software/<lang>/<topic>.md   (writable)
                                                 │
                                                 │ dream gate only:
                                                 │ 3+ memory entries AND
                                                 │ hard prohibition
                                                 ▼
                                 CLAUDE.md "Known Gotchas"  (always-on tier)
```

**This command NEVER writes `CLAUDE.md`.** Promotion to `CLAUDE.md` is *exclusively*
performed by `/reflection:consolidate --dream` under its gate. When an entry looks
CLAUDE.md-worthy, route it to a `rules/standards/` file and note that the dream gate will
consider it later — that is the only path. `promote` also never edits `rules/workflow/`
(human-authored process).

## Step 1: Find the entry in native project memory

```bash
grep -rni "<keywords>" ~/.claude/projects/*/memory/
```

Show the matching topic file(s) + `MEMORY.md` index line and confirm with the user this is
the entry to promote. (Run `/reflection:review` first if you need candidates.)

## Step 2: Route to a target (reuse the `/harness:adapt` routing table)

Pick the writable target by signal type — identical routing to `/harness:adapt`:

| Signal | Target file | Section |
|--------|-------------|---------|
| Code behavior mistake | `rules/software/<lang>/patterns.md` or `rules/standards/quality.md` | language patterns / quality |
| Missing convention | `rules/software/<lang>/coding-style.md` or `rules/standards/quality.md` | conventions / quality |
| Security pattern | `rules/software/<lang>/security.md` or `rules/standards/security.md` | mandatory checks |
| Testing pattern | `rules/software/<lang>/testing.md` or `rules/standards/quality.md` | test conventions |
| Git/commit pattern | `rules/standards/git.md` | commits / branch / PR |
| Docs/communication | `rules/standards/documentation.md` or `communication.md` | docs / response style |
| Performance insight | `rules/standards/performance.md` | relevant subsection |

Replace `<lang>` with the project's primary language dir (`python`, `go`, …). Honor an
explicit `--target <path>` only if it points at a writable tier (never `CLAUDE.md`,
`rules/workflow/`).

## Step 3: Distill into a rule

Transform the memory note (descriptive, multi-line) into a prescriptive one-liner:

- Imperative voice ("Use X", "Never Y").
- Include the concrete command/path/value, not just the concept.
- No backstory — the instruction only.

Append the rule (a line to a short paragraph) under the right heading in the target
`rules/` file. Create the file only if the topic genuinely has no home, following the
existing `rules/standards/` / `rules/software/<lang>/` format — and with **no `paths:`
frontmatter** unless the file is meant to be on-demand.

## Step 4: Remove the entry from native project memory

After the target write lands:

1. Delete the topic file (`rm ~/.claude/projects/<slug>/memory/<topic>.md`) — or, if the fact
   shared a file with still-relevant content, edit out only the promoted portion.
2. Remove its index line from that project's `MEMORY.md`.
3. Confirm the index no longer references a deleted file (no dangling links).

This frees MEMORY.md space (cap: 200 lines auto-loaded) and prevents the same rule living in two tiers.

## Step 5: Report

```
Promoted: "<distilled rule>"
  → <target file> §<section>
Removed from native memory: <slug>.md  +  MEMORY.md index line
MEMORY.md: N index lines remaining

[if CLAUDE.md-worthy] Routed to <rules file>; the /reflection:consolidate --dream
gate (3+ memory entries AND hard prohibition) will consider it for CLAUDE.md promotion.
This command does not write CLAUDE.md.
```

## Promote when / don't

- **Promote** when the entry recurs across sessions, prevents a repeat mistake, or is a
  convention any contributor should know (`/reflection:review` scores it ≥ 6).
- **Don't** promote one-off debugging notes, session-specific context, or anything mid-migration
  that may change soon — leave those in native project memory.
