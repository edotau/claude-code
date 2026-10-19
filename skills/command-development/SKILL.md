---
name: command-development
description: Use when creating slash commands, defining command arguments (argument-hint, $ARGUMENTS, $1/$2), setting allowed-tools, organizing /command files, or referencing CLAUDE_PLUGIN_ROOT in Claude Code plugins.
metadata:
  version: 0.2.0
---

# Command Development for Claude Code

> **Legacy format.** `.claude/commands/*.md` is the legacy component layout. For new work
> prefer the `.claude/skills/<name>/SKILL.md` directory format — both load identically. Reach
> for a command only when you specifically need slash-command mechanics (below).

The shared authoring doctrine — description-as-discovery-surface (CSO), progressive disclosure,
author-for-the-agent (imperative directives, not user-facing descriptions), and least-privilege
tools — lives in the **`claude-best-practices`** skill and applies here unchanged. This stub
covers only what is command-specific.

## Command-specific mechanics

- **`allowed-tools` frontmatter** — restrict tool access per command, e.g.
  `allowed-tools: Read, Grep, Bash(git:*)`. Scope bash tightly (`Bash(git:*)`, not `Bash(*)`).
  Other frontmatter: `description`, `model`, `argument-hint`, `disable-model-invocation`.
- **Positional arguments** — `$ARGUMENTS` captures all args as one string; `$1`, `$2`, … capture
  them individually. Example: `Review PR #$1 with priority $2` against `/review-pr 123 high`.
  `argument-hint: [pr-number] [priority]` drives autocomplete.
- **Inline bash execution** — ``!`command` `` runs bash before Claude processes the prompt, to
  inject context (e.g. ``Files changed: !`git diff --name-only $1` ``). Requires the matching
  `Bash(...)` scope in `allowed-tools`.
- **`@` file references** — `@path` (or `@$1`) inlines file contents; Read must be allowed.
- **Location & namespacing** — `.claude/commands/` (project) vs `~/.claude/commands/` (user) vs
  `plugin/commands/`; subdirectories become `/help` namespaces. Plugin commands can use
  `${CLAUDE_PLUGIN_ROOT}` for portable paths.

## Deeper references

- `references/frontmatter-reference.md` — full field specs, validation checklist, arg patterns.
- `references/plugin-features-reference.md` — `${CLAUDE_PLUGIN_ROOT}`, bash-execution, multi-component workflows.
- `references/testing-strategies.md` — syntax/frontmatter validation.
- `examples/simple-commands.md`, `examples/plugin-commands.md` — worked examples.
