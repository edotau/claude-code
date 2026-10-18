---
description: "Adaptive learning checkpoint — capture corrections, update rules, persist session knowledge before context expires"
---

# /harness:adapt — Adaptive Learning Checkpoint

Focused, fast reinforcement loop that turns this session's corrections and patterns into permanent rules. Lighter than a full `/reflection:consolidate`, triggered automatically at 85% context (the `context-checkpoint` Stop hook) or manually anytime.

## Usage

`/harness:adapt` — run the adaptive learning checkpoint

---

## Process

### Step 1: Scan for Learning Signals

Read the transcript and recent conversation to identify:

1. **User corrections** — anything the user corrected, rejected, or redirected
2. **Gate failures** — lint, test, or hook failures that required fixes
3. **Pattern discoveries** — new conventions, gotchas, or architecture insights
4. **Tool/provider issues** — credential stalls, upstream failures, config problems

For each signal found, classify it:

| Signal Type | Target File | Section |
|-------------|-------------|---------|
| Code behavior mistake | `rules/software/<lang>/patterns.md` or `rules/standards/quality.md` | Language patterns, or general code quality |
| Missing convention | `rules/software/<lang>/coding-style.md` or `rules/standards/quality.md` | Language-specific conventions, or general code quality |
| Security pattern | `rules/software/<lang>/security.md` or `rules/standards/security.md` | Mandatory checks |
| Architecture insight | `docs/memory/bank/<repo-slug>/decisionLog.md` | Dated entry, newest first |
| Hook/tooling issue | `rules/harness/hooks.md` | Hook-specific section |
| Testing pattern | `rules/software/<lang>/testing.md` or `rules/standards/quality.md` | Test conventions |
| Git/commit pattern | `rules/standards/git.md` | Conventional commits, branch/PR workflow |
| Docs/communication pattern | `rules/standards/documentation.md` or `communication.md` | Markdown/living-docs, or response style |
| Performance insight | `rules/standards/performance.md` | Relevant subsection |
| Session-learned fact | `~/.claude/projects/<slug>/memory/` (topic file + `MEMORY.md` TOC entry) | Appropriate topic |
| Project state change | `docs/memory/bank/<repo-slug>/activeContext.md` | "Current Focus" / "Next Steps" |
| Completed work | `docs/memory/bank/<repo-slug>/progress.md` | "What Works" / "What's Next" |

**Routing priority**: Rules files > memory bank > `CLAUDE.md` (last resort, promotion-gated).
`/harness:adapt` writes only to the **writable** tiers — `rules/standards/` and
`rules/software/<lang>/`. It **never** edits `rules/workflow/` (human-authored process), and
**never** writes to `CLAUDE.md` directly — only `/reflection:consolidate --dream` promotes
learnings there after the recurrence + hard-prohibition test passes. Replace `<lang>` with the
project's primary language directory (e.g., `python`, `go`).

### Step 2: Capture Decisions

Scan for architectural or design decisions made this session. For each:

- What was decided and why
- What alternatives were considered
- Where it affects the codebase

Write decisions to `docs/memory/bank/<repo-slug>/decisionLog.md` (dated entry, newest first;
supersede, never delete), or to native project memory if session-scoped.

### Step 3: Route Corrections to Target Files

For each correction identified in Step 1:

1. **Identify the target file** using the routing table above
2. **Read the target file** to check if a similar rule already exists — strengthen it instead of duplicating
3. **Draft a specific, actionable rule** (not vague advice, imperative voice)
4. **Add to the appropriate section** within the target file
5. Keep rules concise: one line to a short paragraph in rules files

**Only update files if there are genuine corrections. Do not add rules for things that already work fine.**

### Step 4: Update the Memory Bank

Update `activeContext.md` (contracts: `rules/workflow/memory-bank.md`; path via `claude-code memory path`):
- **Current Focus**: Reflect where the session ended
- **Next Steps**: Update with immediate next actions
- (Session summaries belong in `sessionHistory.md` — `/memory:end` owns that; skip here)

Update `progress.md` if:
- New functionality was completed ("What Works")
- Priorities shifted ("What's Next")
- New issues discovered ("Known Issues")

### Step 5: Update Native Project Memory

Only write what was actually learned this session. 

1. Read the current `~/.claude/projects/<slug>/memory/MEMORY.md` index (+ linked topic files)
2. Summarize the learning into a topic file, then add/refine its one-line TOC entry in `MEMORY.md`
3. Keep `MEMORY.md` a lean index (first ~200 lines auto-load; detail lives in topic files); never edit the RULES region

### Step 6: Report

Output a compact summary:

```
Adaptive Learning Checkpoint
════════════════════════════
Context: XX% used

Signals captured: N
  - [type]: [brief description] → [target file]

Files updated:
  - Rules: [list of rules/ files updated, or "none"]
  - CLAUDE.md: no (promotion-gated; only /reflection:consolidate --dream writes here)
  - Memory bank (activeContext.md / progress.md / decisionLog.md): [yes/no]
  - Agent memories: [list of agents updated]

Session can now safely end or compact.
```

---

## Auto-Trigger

This command is automatically triggered by the `context-checkpoint` Stop hook when context usage reaches 85%. The hook blocks the stop and instructs Claude to run `/harness:adapt` first.

After `/harness:adapt` completes, Claude may proceed to stop normally.
