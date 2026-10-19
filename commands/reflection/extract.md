---
description: "Turn a recurring auto-memory pattern into a standalone skill under skills/<name>/SKILL.md — drives the opencode agent (EXTRACT mode)."
argument-hint: "<pattern description> [--name <skill-name>] [--dry-run]"
---

# /reflection:extract — Create a Skill from a Pattern

> Part of the `/reflection:*` family (see `/reflection:review`, `:promote`, `:remember`,
> `:consolidate`). Use this when a learning is reusable how-to knowledge rather than a
> one-line rule. Rules go to `/reflection:promote`; deep, on-demand reference material
> becomes a **skill**.

New skills are created in directory form at `skills/<name>/SKILL.md`, following the
structure every skill in this repo already uses: YAML frontmatter (`name` matching the
folder, `description` written for Claude Search Optimization), a third-person/imperative
body, and progressive disclosure (`references/` for depth, `SKILL.md` stays lean).

## When to extract (vs promote)

| Extract a skill | Promote a rule |
|-----------------|----------------|
| Multi-step procedure that's easy to forget | One-line imperative ("Never X") |
| Non-obvious fix that took real debugging | Convention / prohibition |
| Recurs across 2+ projects | Project-specific doctrine |
| User says "save this as a skill" | User says "make this a rule" |

## Step 1: Identify the pattern

```bash
grep -rni "<keywords>" ~/.claude/memory/*/   # one store dir per repo
```

Use the matching topic file(s) as source material if found; otherwise use the user's
description directly. Ask at most two clarifying questions (what problem it solves; whether
to include code examples).

## Step 2: Name the skill

Lowercase, hyphenated, 2–4 words, descriptive (e.g. `provider-auth-refresh`,
`router-fallback-tuning`). Honor `--name` if given. The folder name and the SKILL.md `name:`
frontmatter must match.

## Step 3: Dispatch opencode (EXTRACT mode)

Launch the **opencode** agent (EXTRACT mode) with: the source material, the chosen name, and
the output path `skills/<name>/`. The agent authors `SKILL.md` (plus any `references/` it
needs). With `--dry-run`, it previews the SKILL.md and file list without writing.

## Step 4: Quality gate (opencode EXTRACT enforces, verify in the report)

- Valid YAML frontmatter with `name` + `description`; `name` == folder name.
- `description` carries trigger keywords (Claude search optimization).
- Self-contained — no original-session context needed to follow it.
- No hardcoded secrets, tokens, or machine-specific paths.

## Step 5: Source handling

The native memory entry is **retained** — the skill is portable, the memory note is the
local breadcrumb. (Contrast `/reflection:promote`, which *removes* the entry because a rule
supersedes the note.) If the entry is now fully redundant with the skill, mention it as a
`/reflection:promote`/cleanup candidate but do not delete it here.

## Step 6: Report

```
Skill extracted: <name>
  skills/<name>/SKILL.md (N lines)
  [+ references/*.md if any]
Frontmatter: name=<name> matches folder · description has triggers · OK
Source: <agent>/<slug>.md (retained — skill is portable)
```
