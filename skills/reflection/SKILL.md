---
name: reflection
description: >-
  The memory bank's one entry point — `/reflection:<verb>` → `claude-code memory <verb>`. Bank
  contract, promotion ladder, verb table. Trigger on "recall memory", "remember this",
  "save this session", "promote this to a rule/skill", "memory doctor", "dream consolidation",
  or any `/reflection:*` / `claude-code memory *` dispatch needing judgment.
user-invocable: false
---

# Reflection — The Memory Bank's Entry Point

One store (`~/.claude/memory/<repo>/`, catalog at `memory/index.md`), one family of actions:
`/reflection:<verb>` is a thin launcher for `claude-code memory <verb>`. Go does every mechanical
step; only the judgment residue — text a model must produce — reads the matching
`references/<verb>.md` and is applied back by the CLI (or, on a manual/interactive run, by you
directly editing the files the reference names).

## Bank contract

Each repo's dir (`claude-code memory path`) holds Claude Code's native auto-memory (`MEMORY.md` +
topic files) AND six typed bank files, side by side:

| File | Holds | Cap |
|---|---|---|
| `projectContext.md` | Orientation + do-not-regress invariants | 60 lines |
| `activeContext.md` | Current focus, open questions, blockers, next steps | 80 lines |
| `progress.md` | What Works / In Progress / What's Next / Known Issues | 60 lines |
| `decisionLog.md` | Dated decisions, newest first, supersede-never-delete | 200 lines |
| `conventions.md` | Learned patterns not yet promoted | 80 lines |
| `sessionHistory.md` | Newest-first session summaries | 30 entries (overflow archives) |

`MEMORY.md`'s `<!-- bank:begin -->`…`<!-- bank:end -->` region is generated — never hand-edit it or
`memory/index.md`. The approval-gated `<!-- RULES:BEGIN -->`…`<!-- RULES:END -->` region in native
`MEMORY.md` is never edited without explicit human approval, by any verb.

**Settle** = `claude-code memory index` — rotates `sessionHistory.md` past its cap into
`archive/`, rewrites the bank region, regenerates the catalog. Deterministic, safe to re-run;
every writer verb ends by settling.

## Promotion ladder

```text
bank entry (conventions.md / decisionLog.md / native topic file)
        │  claude-code memory promote --to home
        ▼
  home bank (~/.claude/memory/home/) ── cross-project, not yet a rule
        │  claude-code memory promote --to rules | --to skill
        ▼
  rules/standards/<topic>.md  or  rules/software/<lang>/<topic>.md   (writable, enforced)
  skills/<name>/SKILL.md                                              (reusable how-to)
        │  dream gate only: 3+ bank entries AND a hard prohibition
        ▼
  CLAUDE.md "Known Gotchas"   ← always-on tier, reached only via `/reflection:dream`
```

`--to claude-md` does not exist as a direct target — CLAUDE.md is reached only through
`/reflection:dream`'s cross-pollination gate. `promote` never edits `rules/workflow/` (human-authored
process).

## Verb table

| Verb | Access | Does | Judgment (`references/<verb>.md`) |
| --- | --- | --- | --- |
| `recall <task>` | public | Search → top-k ids + one-line previews; `--load N` appends bodies under a byte cap | none |
| `remember <fact> [--type]` | public | Write a typed entry to the right bank/native file, id marker, settle | which `metadata.type` fits ([remember.md](references/remember.md)) |
| `init` | public | Scaffold the bank | OPTIONAL enrichment from repo state ([init.md](references/init.md)) |
| `review [--brief\|--stale\|--candidates]` | public | Caps, staleness, exact dupes, hot/cold from `access.log` (folds the old `doctor`/`stats`) | `--candidates`: rank promotion candidates ([review.md](references/review.md)) |
| `promote <id> --to home\|rules\|skill\|claude-md` | public | Move/copy the entry, link, drop the source, settle | target wording for `rules`/`skill`; `claude-md` is dream-gated ([promote.md](references/promote.md)) |
| `update` | public | Collect commits, touched files, DECISION/NEXT tags; write the files; settle | session narrative + focus ([update.md](references/update.md)) |
| `dream [--dry-run]` | public | Rotate, archive, exact dedupe, cap enforcement | merge near-duplicates in meaning ([dream.md](references/dream.md)) |

Private plumbing — dispatchable but off the menu, no slash command: `index`, `watch`, `fmt`,
`migrate`, `alias`, `path`, `show`, `digest`, `timeline`. Hooks and background jobs call these
directly; an agent calls them by hand for repair, never through `/reflection:`.

## Before a non-trivial task

Run `claude-code memory recall <task terms>` and load only what you need with
`claude-code memory show <id>` — cheaper than pulling every candidate block into context up front.

## Who writes what

- `session-start` (SessionStart hook) — read-only: injects the catalog + this repo's focus.
- `memory-recall` (UserPromptSubmit hook) — injects up to a few matching blocks per owner prompt.
- `agent-effort` (PreToolUse Agent hook) — appends up to 3 recall pointers to a subagent's spawn prompt.
- `session-harvest[-end]` / `pre-compact` — background dispatch of `claude-code memory update` so a
  session that forgets to save still gets persisted.
- `/reflection:update` — the primary interactive writer at session close.
- `/reflection:dream` — periodic hygiene; the only path to `CLAUDE.md`.
