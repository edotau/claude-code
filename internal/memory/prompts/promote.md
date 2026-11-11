# `memory promote --to rules|skill|claude-md` — Judgment Residue: Target, Wording, Gate

> The daily background `claude-code memory dream` also prints this during its cross-pollination step.

`claude-code memory promote <id> --to home|rules|skill|claude-md` locates the entry. `home` moves it
to the home bank (no link back) and settles; `rules`, `skill` and `claude-md` only print the block, its target and
this procedure, and you edit the target by hand. What the verb cannot do is **pick the right target**, **word a
rule or skill so it stands alone**, or **gate `claude-md`**. That's this reference.

## The promotion gradient

Full ladder: `skills/reflection/SKILL.md` → Promotion ladder. In short: `home` (cross-project bank) → `rules`
(`rules/standards/` or `rules/software/<lang>/`, enforced) or `skill` (`skills/<name>/SKILL.md`, how-to) → `claude-md`
(`CLAUDE.md` → Hard rules, always on).

**`--to claude-md` is dream-gated:** the verb refuses outside `memory dream`'s cross-pollination step, which
requires 3+ memory entries AND a hard prohibition. Elsewhere, route a CLAUDE.md-worthy entry to `rules/standards/`
instead and note that the dream gate will consider it later. `promote` also never targets `rules/workflows/`
(human-authored process).

## `--to home`: durable + cross-project

1. **Resolve the store** — `claude-code memory path` for this repo's dir; the home dir is
   `memory/home/` (`claude-code memory path` from any non-repo cwd). Never hardcode either.
2. **Read the local dir** — `conventions.md` and `decisionLog.md` are the two append-only files
   that accumulate durable lessons; `activeContext.md`/`progress.md`/`sessionHistory.md` are
   session state and never promote.
3. **Judge each candidate entry** — promote ONLY if it is:
   - **durable + cross-project** — a truth that holds beyond this one repo (not project state
     like "this repo's build is flaky", which stays local);
   - **not already** present in the home bank (`grep` it there first).
   Drop anything project-specific, one-off, or already captured in the home bank.
4. **Run** `claude-code memory promote <id> --to home`, then `claude-code memory index`.

**Promote** when the lesson would help a session in a *different* repo, or is infrastructure
doctrine (a provider quirk, a router behavior, a hook contract) rather than this repo's state.
**Don't** promote anything still mid-change, or specific to this repo's own code.

## `--to rules`: durable, enforceable doctrine

### Step 1: Find the entry

```bash
grep -rni "<keywords>" ~/.claude/memory/*/   # one store dir per repo
```

Show the matching topic file(s) + `MEMORY.md` index line and confirm this is the entry to
promote. (Run `claude-code memory review` first if you need candidates.)

### Step 2: Route to a target

Pick the writable target by signal type with the table below. Replace `<lang>` with the project's
primary language dir (`python`, `go`, …). Honor a target the user names only if it is
a writable tier (never `CLAUDE.md`, `rules/workflows/`).

| Signal Type | Target File | Section |
|-------------|-------------|---------|
| Code behavior mistake | `rules/software/<lang>/patterns.md` or `rules/standards/quality.md` | Language patterns, or general code quality |
| Missing convention | `rules/software/<lang>/coding-style.md` or `rules/standards/quality.md` | Language-specific conventions, or general code quality |
| Security pattern | `rules/software/<lang>/security.md` or `rules/standards/security.md` | Mandatory checks |
| Architecture insight | `~/.claude/memory/<repo>/decisionLog.md` | Dated entry, newest first |
| Hook/tooling issue | `rules/harness/hooks.md` | Hook-specific section |
| Testing pattern | `rules/software/<lang>/testing.md` or `rules/standards/quality.md` | Test conventions |
| Git/commit/worktree pattern | `rules/standards/worktree-git.md` | Conventional commits, branch/PR workflow |
| Docs/communication pattern | `rules/standards/documentation.md` or `rules/standards/principles.md` | Markdown/living-docs, or response style |
| Model-selection insight | `rules/harness/providers.md` | Model selection |
| Session-learned fact | `~/.claude/memory/<repo>/` (topic file + `MEMORY.md` TOC entry; dir from `claude-code memory path`) | Appropriate topic |
| Project state change | `~/.claude/memory/<repo>/activeContext.md` | "Current Focus" / "Next Steps" |
| Completed work | `~/.claude/memory/<repo>/progress.md` | "What Works" / "What's Next" |

**Routing priority**: Rules files > memory bank > `CLAUDE.md` (last resort, promotion-gated). This
path writes only to the **writable** tiers — `rules/standards/` and `rules/software/<lang>/`. It
never edits `rules/workflows/` (human-authored process), and never writes to `CLAUDE.md` directly —
only `memory dream` promotes learnings there after the recurrence + hard-prohibition test passes.

### Step 3: Distill into a rule

Transform the memory note (descriptive, multi-line) into a prescriptive one-liner:

- Imperative voice ("Use X", "Never Y").
- Include the concrete command/path/value, not just the concept.
- No backstory — the instruction only.

Append the rule (a line to a short paragraph) under the right heading in the target `rules/`
file. Create the file only if the topic genuinely has no home, following the existing
`rules/standards/` / `rules/software/<lang>/` format — and with **no `paths:` frontmatter** unless
the file is meant to be on-demand.

**Promote** when the entry recurs across sessions, prevents a repeat mistake, or is a convention
any contributor should know (`memory review` scores it ≥ 6). **Don't** promote one-off
debugging notes, session-specific context, or anything mid-migration that may change soon.

## `--to skill`: reusable how-to, not a one-liner

Use when a learning is reusable how-to knowledge rather than a one-line rule.

| `--to skill` | `--to rules` |
|-----------------|----------------|
| Multi-step procedure that's easy to forget | One-line imperative ("Never X") |
| Non-obvious fix that took real debugging | Convention / prohibition |
| Recurs across 2+ projects | Project-specific doctrine |
| User says "save this as a skill" | User says "make this a rule" |

### Step 1: Identify the pattern

```bash
grep -rni "<keywords>" ~/.claude/memory/*/   # one store dir per repo
```

Use the matching topic file(s) as source material if found; otherwise use the user's description
directly. Ask at most two clarifying questions (what problem it solves; whether to include code
examples).

### Step 2: Name the skill

Lowercase, hyphenated, 2–4 words, descriptive (e.g. `provider-auth-refresh`,
`router-fallback-tuning`). Honor a name the user gives. The folder name and the SKILL.md `name:`
frontmatter must match. Verify no collision with `Glob`.

### Step 3: Write the skill

From the source material, author `skills/<name>/SKILL.md` (plus any `references/` it needs).
Show the user the SKILL.md and file list before writing.

### Step 4: Quality gate

- Valid YAML frontmatter with `name` + `description`; `name` == folder name.
- `description` carries trigger keywords (Claude search optimization).
- Self-contained — no original-session context needed to follow it.
- No hardcoded secrets, tokens, or machine-specific paths.

**Portability test**: "Would this help someone in a project I've never seen?" Harness-only skills
are allowed but must scope themselves in the description. Naming anti-patterns:
`my-repo-pat-fix` (project-specific) → name the mechanism; `debugging` (too generic) →
`root-cause-tracing`; `fix`/`skill` (no problem stated) → name the actual problem.

### Step 5: Source handling

The native memory entry is **retained** — the skill is portable, the memory note is the local
breadcrumb, unless the entry is now fully redundant with the skill (then flag it as a
`--to home`/cleanup candidate, but don't delete it here).

## Step N (every target): remove the entry from native memory

After the target write lands and `claude-code memory promote` runs (or the by-hand move for an
entry under cap):

1. Delete the topic file (memory git keeps its history) — or, if the fact shared a file with
   still-relevant content, edit out only the promoted portion.
2. Remove its index line from that project's `MEMORY.md`.
3. Confirm the index no longer references a deleted file (no dangling links).
4. Run `claude-code memory index`.

This frees `MEMORY.md` space (caps: `rules/workflows/memory-bank.md` → Caps) and stops one rule living in two tiers.

## Report

```text
Promoted: "<distilled rule / entry title>"
  → <target> (home bank | rules/<file> §<section> | skills/<name>/SKILL.md)
Removed from native memory: <slug>.md  +  MEMORY.md index line
MEMORY.md: N index lines remaining (of 200)

[if claude-md-worthy] Routed to <rules file>; the `memory dream` gate
(3+ memory entries AND hard prohibition) will consider it for CLAUDE.md promotion.
This path does not write CLAUDE.md directly.
```

## What you don't do

- Don't create agent files; don't invent patterns; don't create omnibus guides (one problem per
  skill); don't write discovery-narrative history.
- Don't write `CLAUDE.md` outside `memory dream`'s gate.

## Failure Modes

- **Pattern too vague (`--to skill`)** — ask for the exact error, repro steps, and fix before writing.
- **One-time debug note (`--to skill`)** — if it fails the "would this help a stranger" test, suggest
  native project memory instead.
- **Existing skill covers it (`--to skill`)** — report the match and propose extending it.
