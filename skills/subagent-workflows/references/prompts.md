# Dispatch Prompts

Templates for `../SKILL.md` → Phase 1 → Handoff contract (same fields); paste each role's context, never make it
rebuild the plan. `Agent: in-context` = the leaf does it itself.

## Handoff contract details

**Brief (in)** — a fresh agent can act from it alone (the CLI starts cold and cannot see this session):

- `Objective:` one outcome. `Agent:` the work agent; omit for the selected type, `in-context` for direct work; keep
  it consistent with the type (no hidden relay). The hook reads `Agent:` only before the first blank line.
- `Model:` is not parsed by the hook: put overlay `model` on the carried ask
  (`agents ask --carry --overlay '{"agent":"<a>","model":"m"}'`).
- `Mode:`, `Owns:`, `Method:`, `Verify:`, `Acceptance:` are conventions, not routing hooks. `Task:` identifies a plan
  task for the hook's slot claim (`Task: Phase W.k`): the hand-back then carries its `verify:` result. `Tier:
  fast|standard|deep` (own line) sets the task tier that picks an unpinned run's model (`agent.TaskTier`); without it
  the wording decides. A gate-run leaf that only reports carries `Tier: fast`.
- `Owns:` absolute paths and forbidden files; the report lists the files actually changed. `Method:` the skill.
  `Context:` evidence, decisions, plan path. `Constraints:` applicable rules and the shared-tree rule.
  `Verify:` a scoped command; `Acceptance:` its observable outcome. `Workflow:` a saved Workflow, only when requested.
- A type with no agent file (`general-purpose`, `Explore`) gets no leaf rules: paste the `## Leaf rules` lines of
  `internal/assets/brief/brief.tmpl` (no foreground `sleep`, no git or `sudo` probes in `~/.claude`) into its brief.
- State what to report on a missing credential, a red gate, an unowned file or a dependency.
- `claude-code agents brief --task F --own P…` prints the brief: `Owned:`, mode, your task, the leaf rules and the
  reporting contract from `internal/assets/brief/brief.tmpl`; never restate that footer. Pipe it to `agents ask -`;
  `agents diff --verify <id>` reads `Owned:` back.
- Overlay `profile` leads then falls back unless `"fallback":"none"` pins it; carried asks carry the route as
  `--overlay '<object>'` (provider and profile together; an inherited `COWORKER_OVERLAY` carries a child's derived
  confinement). Overlay keys: `rules/harness/agent.md` → Overlay; `--remote` vs `--host`: `rules/harness/network.md`.

**Report (out)** — verdict of at most 8 lines: a review's verdict word, else one Status: `DONE` ·
`DONE_WITH_CONCERNS` (resolve before review) · `NEEDS_CONTEXT` (return to owner) · `BLOCKED` (change something; never
resend unchanged); then `Ran on:` (the `ask: ran on …` line; `self` only for `claude`, `Explore`, `Plan` or an
`Agent: in-context` brief), outcome per item, test or gate results, what needs a decision, and the report path. The
full report (root cause, before/after output, numstat, teeth proof, commands) goes to
`$(claude-code memory path reports YYYY-MM-DD-<name>.md)`. Scratch files only under `/tmp/<job>/`, removed before
reporting.

**Ownership** — one writer per file, split by file not topic; one package parallelizes when each leaf creates its own
new file and reports its API surface; the manager makes the integration edit and owns decisions. Leaves remain
terminal (an external `ask` is allowed). Commit authority: `rules/harness/workflow-tree.md` → Worktrees.

## Every brief adds

```text
Agent: <in-context | pi (default for code leaves) | work agent matching the selected leaf type>
Shared tree: follow CLAUDE.md hard rule 4 and rules/harness/workflow-tree.md.
Confirm each write with `rtk git diff --stat -- <file>`.
Terminal leaf: never dispatch subagents or commit; external wrappers follow the shared relay contract.
Before a behavior change write TASK: code does X; check expects Y; spec says Z (Y vs Z disagree: fix the side the spec
contradicts, or stop and report). After a defect fix write ISSUE: searched <pattern> — found <N> other sites:
<files|none>; before reporting PENDING: <skipped follow-up or none>. Report all as plain sentences.
Return: status DONE | DONE_WITH_CONCERNS | NEEDS_CONTEXT | BLOCKED,
Ran on: <work receipt; self only for claude, Explore, Plan or Agent: in-context>.
After a Pi fallback, include the failed Pi line before self.
```

Templates list only their own lines. Reviewers inspect the artifact, never the report about it.
Use Spec Reviewer and Code Quality Reviewer as lenses of the shared collector review, after simplify.

## Implementer

```markdown
Objective: implement <name>: <complete requirements>.
Task: <plan task id; omit with no plan>
Context: <working directory, files, dependencies, prior decisions>
Owns: <files>. Do NOT edit others — agents are in this package concurrently.
Acceptance: <testable outcomes>
Verify: `<scoped command>`
Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 4-6)
NEEDS_CONTEXT before editing if unclear; this task only, minimally. Return also: files changed; verification; concerns.
```

## Researcher

```markdown
Objective: answer <question> for <caller requirement>.
Context: <facts already established, links supplied>; do not re-derive. Sources: <primary sources to prefer>.
Budget: <requests or minutes>; stop at it and return what you have.
Tools: <exact tool names and auth state>. On an auth failure or denial, stop and report the tool and the redacted error
— no unchanged retry, no other tool around it. Links: after a 404, search for the real link, never guess.
Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 0-2). Return also: each requirement → evidence or the gap.
```

## Spec Reviewer

```markdown
Specification: <requirements and acceptance criteria>. Implementation report: <implementer report>.
Scope: <base..head or changed files>. Find: missing or misread requirements; unrequested behavior or scope;
unsupported completion or test claims. No style review. Return SPEC_COMPLIANT with brief evidence, or SPEC_ISSUES
with file:line findings.
```

## Simplifier

```markdown
Simplify, behavior-preserving: `Skill(simplify)` under `rules/standards/quality.md` → Simplify (a terminal leaf runs
its four angles itself). Scope: base <rev>, head <rev>. May edit: <changed files>; an out-of-diff edit only with
its blast radius in the report. Cheap check, before and after: `<command allowed by testing-guidelines.md>`
Return also: the simplify report (Applied file:line + assumption · Flagged · Bugs found · Skipped; Δ lines and
Δ complexity per touched function); gate output before/after; DONE (applied N, flagged M) | BLOCKED (gate red first).
```

## Code Quality Reviewer

```markdown
Apply ~/.claude/skills/evaluate-code/SKILL.md's Lenses and Verdicts (performance lens on any hot path).
Objective: review <summary>. Scope: base <rev>, head <rev>, files <list>. Simplify report: <report>
Only issues this change introduced (complexity is the simplify lane). Return SHIP | FIX REQUIRED | BLOCK first, then
CRITICAL / HIGH / MEDIUM / LOW findings with file:line and a fix.
```

## Plan Reviewer

```markdown
Review plan <PLAN_PATH> against spec <SPEC_PATH> by `~/.claude/skills/evaluate-code/references/task-review.md` →
Plan review. Return its verdict first.
```

## Lane leaf

```text
Task: <plan task id>
Objective: <checklist line, verbatim>. Edit only: <owned files>. Scratch: /tmp/<job>/; delete it when done.
Done when: <cheap check or exact-name repaired/written test> passes and <live check> shows <expected>.
Return also: files changed, commands with exit
code and env (cwd, CLAUDE_CONFIG_DIR), anything outside your files.
```
