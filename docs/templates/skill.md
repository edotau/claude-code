---
name: <skill-stem>
description: '<What it does, one clause>. Use for "<trigger>", "<trigger>", <situations>.
Not for <neighbour skill or task>.'
---

# <Skill title>

<One or two lines: what the skill is for and what it never does (for example "never commits").>

## When to use

- <Situation that routes here; name the inputs you expect.>
- **Not for:** <task that belongs to another skill, agent or verb, with its name>.

| Step needs                           | Load                    |
| ------------------------------------ | ----------------------- |
| <Detail the main path does not need> | `references/<topic>.md` |

## Workflow

1. <Scope: what to read or run first, as a command in its rtk form.>
2. <Act: the smallest change or check that does the job.>
3. <Name each assumption before a cut or a claim; no stated assumption means a flag, not a fix.>

## Verify

- <Observable check: a command, a test or a number that must pass before and after.>
- <Say which checks ran and which did not.>

## Report

- Lead with the outcome; then what changed (`/absolute/path:line`), what was skipped and why.

<!--
Authoring notes (delete from the new file):
- `name:` equals the directory stem; `description:` carries the triggers because nothing else loads upfront.
- One home per procedure: link rules and other skills, never copy them; heavy detail goes in `references/`,
  shell logic in `scripts/` (`rules/workflows/agents.md` → Agent and command files).
- Third-party source is fetched into a gitignored dir at runtime and never committed (public repo, license).
- Prose lines at most 120 columns; comments one line (`rules/standards/quality.md` → Comments).
-->
