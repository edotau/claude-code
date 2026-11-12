# Agent Design Patterns (this harness)

Every row maps to code this repo runs. Serves `../SKILL.md` Phase 0: pick the shape here, fill each node with a leaf
type, then wire stages into a template from [composites.md](composites.md). Commit hashes below are the evidence.

## Execution mode: subagents, plus scripts that hold the plan

Agent teams (`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`) are not used; the harness keeps leaves terminal. Plan
ownership: `../SKILL.md` → Who holds the plan.

Workflow limits (docs): 16 concurrent agents by default, 1,000 per run, no mid-run input, run a small slice first
(`code-workers` `slice`). The dispatch cap (`rules/harness/hooks.md` → dispatch cap) claims one slot per `agent()`
site in a script (`internal/hooks/agents/leaves.go` `Effort`).

## Patterns we run

| Pattern | Harness component | Roles filled by | Enforced by |
| --- | --- | --- | --- |
| Pipeline | `code-workers.js` per package | find, rederive, fix, test, gate = Sonnet/Haiku claude agents | `stepOf` pins, schemas |
| Fan-out / fan-in | `code-workers.js` `pipeline(PKGS, review, repair)` · parallel leaves | `claude` leaves, disjoint files | dispatch cap, `worktree-claim-check` |
| Expert pool | `code-engineer.js` sizing router | `haiku` sizer picks the route | tier table, `dispatch-effort` |
| Producer-reviewer | fix, guard test, gate · rederive | implementer vs blind re-deriver | guard test and gate exit codes |
| Supervisor | main session + leaves | `claude`, `codex`/`copilot`/`gemini`/`opencode`, `Explore` | dispatch cap, `dispatch-stop` ran-on |
| Hierarchical | `code-engineer.js` calling `workflow()` on `code-workers.js` | one workflow level | runtime rule: one nesting level |

### Pipeline

```text
package p:  find(2 lenses) -> merge(file +-10 | symbol) -> rederive(blind) -> fix -> guard test -> gate(p)
```

- Code: `~/.claude/workflows/code-workers.js` (`review`, `repair`). Gate is package-scoped plus
  `internal/schemas` when a writer changed.
- Evidence: R9 26 agents / 2.0M tokens, yield 7.5 findings per 1M; R10 60 / 4.8M (7.1); R11 114 / 8.8M (7.0).

### Fan-out / fan-in

```text
packages -> [ code-workers stream p1 | p2 | ... | pN ]  (no barrier)  -> merged report
```

- code-workers streams each package to its fixer the moment its findings are confirmed; the barrier shape (all
  reviews, then all fixes) is retired. Finders in `code-analyzer.js` remain a fan-out over (dimension x chunk) with a
  refute step.
- Manual form: `Agent` calls in one message with `Owns:` files; fan-in reads every diff and the seam between leaves
  (`../SKILL.md` Phase 3).
- Failure: wording-based dedupe produced duplicate findings (`code-analyzer` yield 2.8 and 4.0); code-workers merges by
  file +-10 lines or same symbol. Loop-until-dry never converged (119 agents / 10M, yield 2.2): retired.

### Expert pool

```text
task -> size(TRIVIAL|LOW|MEDIUM|HIGH|EPIC) -> code-workers x<=3 | code-analyzer (bare range) | plan draft
```

- Code: `code-engineer.js` (Size, Route, then Simplify and Review once over the children's merged fixes, Record).
  Tiers follow `rules/workflows/master-workflow.md` Phase 0; `tier` skips sizing.
- Specialists by model alias: Haiku sizes, gates and records; Sonnet finds, fixes and tests; `opus`/`fable` only by a
  `steps` pin.

### Producer-reviewer

```text
fix -> test agent writes guard test -> gate
finder claim -> blind re-derivation from file:line + symptom -> reproduced | dropped | unverified
```

- A re-deriver never sees the finder's reasoning. Guard test: `rules/standards/testing-guidelines.md`.
- Owner mid-turn messages reaching workflow agents is an open defect.

### Supervisor

```text
main session -> Explore x3 -> Plan -> claude || copilot (test agent) -> fan-in -> verify -> commit
```

- Ownership: `../SKILL.md` → Handoff contract. Run-state: `claude-code agents tree`; proof an external agent ran:
  `ask: ran on` line (`externalTypes`, `internal/hooks/agents/leaves.go`).

### Hierarchical

```text
main -> Workflow code-engineer -> workflow(code-workers | code-analyzer) -> in-script stages -> agent() leaves
main -> Workflow code-analyzer -> Scan -> Plan (script) -> waves -> in-script stageGate
```

- `code-engineer` returns its children's merged result plus tier, route, polish, files and `recorded` paths.

### Sequential plan and tournament

- Sequential plan: per task implementer, spec review until `SPEC_COMPLIANT`, `Skill(simplify)`, quality review until
  `SHIP` ([prompts.md](prompts.md)); stop and ask when an assumption breaks. Ticks via `docs-plan-tick`.
- Tournament: after the owner approves worktree creation, baseline the eval, send one brief verbatim to N `claude`
  leaves (`isolation: "worktree"`, branch `compete/<slug>/<n>`), re-measure yourself, `claude-code worktree merge` the
  winner, tag losers. A tie is not a win.

### Not used

- Agent teams / `TeamCreate`: experimental, one per session, no nesting.
- Loop-until-dry and the modal sweep: yield 2.2 and 2.5, retired or pre-pass only.
- External workflow stages and supported overrides: [saved-workflows.md](saved-workflows.md).
- Peer-to-peer agent debate: no harness component; refute/rederive is one-way and blind.

## Split vs merge; skill vs agent vs brief

Agent file vs skill vs brief, and when to split or merge a role: `rules/workflows/agents.md` → Authoring: file, skill
or brief; the file skeleton and frontmatter are in its Agent and command files section. Hook tables a new
external-agent type must join: same file → Prose instructs, hooks guarantee.

## Lessons from the trial

- Stream per package, no barrier between review and fix (`06ac070e`).
- Record in-run, not in a hook: PostToolUse(Workflow) fires at launch (`2e9a90ff`).
- Re-derive blind, and treat a failed re-derivation as unverified, never refuted (`222ae2b9`).
- A fixer that is not `done` is red even when the gate passes (`07f86477`).
- Route findings to the package that owns the file (`d51420fb`).
- Scoped gates can miss integration failures (`16d36662` broke `internal/ui/menu`); release checks: `CLAUDE.md`
  hard rule 5.
- Revert a leaf's out-of-scope hunk; a leaf never "fixes" files outside its list.
- Pin `model` and `effort` on every stage with one verbatim `stepOf` (`222ae2b9`).
- Fix every finding, LOW included, in the round; only owner-judgment items go to a report.
