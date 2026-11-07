# Agent Design Patterns (this harness)

Every row maps to code this repo runs. Serves `../SKILL.md` Phase 0: pick the shape here, fill each node with a leaf
type (below), then wire stages into a template from [composites.md](composites.md). Trial evidence:
`memory/claude-code/reports/2026-11-07-workflow-scorecard.md` (ranking, yields) and
`2026-11-07-workflow-trial-decisions.md`
(decision numbers `#n` below); current-docs check: `2026-11-07-agent-design-research.md`.

## Execution mode: subagents, plus scripts that hold the plan

Agent teams are experimental (`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`), one team per session, no nesting, interactive
only ([docs](https://code.claude.com/docs/en/agent-teams)); not used (#3). Subagents may spawn subagents to depth 3
(`CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH`); the harness keeps leaves terminal. Start with the simplest shape that works
([Building effective agents](https://www.anthropic.com/engineering/building-effective-agents)); multi-agent costs about
15x a chat and coding has fewer parallel tasks than research
([multi-agent research system](https://www.anthropic.com/engineering/multi-agent-research-system)).

| Who holds the plan | Harness form | Use when |
| --- | --- | --- |
| Claude turn by turn | one leaf, parallel leaves in one message, `Plan` checklist | the next step depends on the last |
| The script | `/home/edotau/.claude/workflows/*.js` ([saved-workflows.md](saved-workflows.md)) | steps known up front, same every time |
| A lead over peers | agent teams | never here |

Workflow limits (docs): 16 concurrent agents by default, 1,000 per run, no mid-run input, run a small slice first
(`relay3` `slice`). The dispatch cap (24 per account, 4 x CPUs machine-wide) claims one slot per `agent()` site in a
script (`internal/hooks/agents/leaves.go` `Effort`).

## Patterns we run

| Pattern | Harness component | Roles filled by | Enforced by |
| --- | --- | --- | --- |
| Pipeline | `relay3.js` per package | find, rederive, fix, test, gate = Sonnet/Haiku claude agents | `stepOf` pins, schemas |
| Fan-out / fan-in | `relay3.js` `pipeline(PKGS, review, repair)` · parallel leaves | `code-worker` leaves, disjoint files | dispatch cap, `worktree-claim-check` |
| Expert pool | `orchestrate.js` sizing router | `haiku` sizer picks the route | tier table, `dispatch-effort` |
| Producer-reviewer | fix, guard test (`test mutate`), gate · rederive | implementer vs blind re-deriver | `test mutate` exit 0, gate exit codes |
| Supervisor | main session + leaves | `code-worker`, `coworker:*`, `Explore` | dispatch cap, `dispatch-stop` ran-on |
| Hierarchical | `orchestrate.js` calling `workflow()` on `relay3.js` | one workflow level | runtime rule: one nesting level |

### Pipeline

```text
package p:  find(2 lenses) -> merge(file +-10 | symbol) -> rederive(blind) -> fix -> guard test -> gate(p)
```

- Code: `/home/edotau/.claude/workflows/relay3.js` (`review`, `repair`). Gate is package-scoped plus
  `internal/schemas` when a writer changed.
- Evidence: R9 26 agents / 2.0M tokens, yield 7.5 findings per 1M; R10 60 / 4.8M (7.1); R11 114 / 8.8M (7.0).
- Failures and fixes: a fixer limited to its package skipped findings cited elsewhere (`d51420fb`); a blocked fixer
  plus a green gate over unchanged code read as fixed (`07f86477`); scoped gates miss cross-package breakage
  (`16d36662` broke `internal/ui/menu`, #24): full `go build/vet/test ./...` before every push (#27).

### Fan-out / fan-in

```text
packages -> [ relay3 stream p1 | p2 | ... | pN ]  (no barrier)  -> merged report
```

- relay3 streams each package to its fixer the moment its findings are confirmed; the older barrier shape
  (`wf-relay2`: all reviews, then all fixes) is retired (scorecard rank 3). Finders in `code-analyzer.js` remain a
  fan-out over (dimension x chunk) with a refute step.
- Manual form: `Agent` calls in one message with `Owns:` files; fan-in reads every diff and the seam between leaves
  (`../SKILL.md` Phase 3).
- Failure: wording-based dedupe produced duplicate findings (`code-analyzer` yield 2.8 and 4.0); relay3 merges by
  file +-10 lines or same symbol. Loop-until-dry never converged (119 agents / 10M, yield 2.2): retired.

### Expert pool

```text
task -> size(TRIVIAL|LOW|MEDIUM|HIGH|EPIC) -> relay3:paths | relay3:packages | plan draft
```

- Code: `orchestrate.js` (Size, Route). Tiers follow `rules/workflows/master-workflow.md` Phase 0; `tier` skips
  sizing.
- Specialists by model alias: Haiku sizes, gates and records; Sonnet finds, fixes and tests; `opus`/`fable` only by a
  `steps` pin. Under Claude quota pressure the hook downshifts one alias (`dispatch-effort`).
- Failure: untested routes (LOW/MEDIUM, implementer, EPIC) at R11; LOW route first run `2026-11-07-route-test-low.md`.

### Producer-reviewer

```text
fix -> test agent writes guard test -> claude-code test mutate (exit 0 = caught) -> gate
finder claim -> blind re-derivation from file:line + symptom -> reproduced | dropped | unverified
```

- A re-deriver never sees the finder's reasoning; a null result is `unverified`, never refuted (`222ae2b9`).
- Guard test: `rules/standards/testing-guidelines.md`; `code-engineer` lacked this stage (green gate, zero new tests).
- Out-of-package fixes and owner mid-turn messages reaching workflow agents are open defects (scorecard).

### Supervisor

```text
main session -> Explore x3 -> Plan -> code-worker || copilot (test agent) -> fan-in -> verify -> commit
```

- The main session decides, integrates and commits; leaves never dispatch, commit or run the full suite
  (`../SKILL.md` Ownership). Run-state: `claude-code agents tree`; proof an external agent ran: `ask: ran on` line
  (`ranOnGated`, `internal/hooks/agents/leaves.go`).

### Hierarchical

```text
main -> Workflow orchestrate -> workflow(relay3) -> agent() leaves      (workflow nests one level)
```

- `orchestrate` returns relay3's result plus tier, route and `recorded` paths. PostToolUse(Workflow) fires at launch,
  so recording happens in-run by a final Haiku agent (`2e9a90ff`, #23).

### Sequential plan and tournament

- Sequential plan: per task implementer, spec review until `SPEC_COMPLIANT`, `Skill(simplify)`, quality review until
  `SHIP` ([prompts.md](prompts.md)); stop and ask when an assumption breaks. Ticks via `docs-plan-tick`.
- Tournament: baseline the eval, send one brief verbatim to N `code-worker`s (`isolation: "worktree"`, branch
  `compete/<slug>/<n>`), re-measure yourself, `claude-code worktree merge` the winner, tag losers. A tie is not a win.

### Not used

- Agent teams / `TeamCreate`: experimental, one per session, no nesting.
- Loop-until-dry and the modal sweep: yield 2.2 and 2.5, retired or pre-pass only (scorecard 7-8).
- External agents inside workflows: `stepOf` rejects `agent`/`profile` (`e1ef8731`); run them outside via
  `/code-worker:codex`.
- Peer-to-peer agent debate: no harness component; refute/rederive is one-way and blind.

## Agent type selection

| Need | Type (`subagent_type`) | Model / effort | Notes |
| --- | --- | --- | --- |
| Read-only scan before work | `Explore` | built-in | scope to touched dirs |
| Design the checklist | `Plan` | `inherit` | plan only; `agents/Plan.md` |
| In-context judge or integrator | `claude` | `inherit` | `agents/claude.md` |
| Default leaf, one scoped task | `code-worker:claude` | `sonnet` / medium | bare `code-worker` runs the brief's `Agent:` |
| Diff or plan review, other family | `code-worker:codex` | pinned in agent file | verdict word |
| Read-heavy sweep, 1M context | `code-worker:gemini` · `gemini` | `sonnet` / low | cannot write |
| Mechanical edits, test repair | `coworker:copilot` · `copilot` | `sonnet` / medium | experimental |
| Edits on discovered models | `coworker:opencode` | `sonnet` / medium | experimental |
| Any agent on a Databricks profile | `coworker:databricks` | per profile | blocked without its secret |
| Workflow stage | `agent()` with `model`+`effort` | `haiku\|sonnet\|opus\|fable`, `low..max` | claude only |

Escalate by lowering `effort` before changing model; after one failed high-effort Sonnet attempt re-dispatch on `opus`.
Alias guidance: aliases or `inherit`, never dated ids on first-party use (research report; `rules/workflows/agents.md`).

## Split vs merge; skill vs agent vs brief

- Agent file = who (role reused across sessions); skill = how (named in a brief's `Method:`); brief = this task. A
  one-off role is a brief to `code-worker`.
- Split only when expertise, tool grant or authority differs (read-only `Explore` vs editing `code-worker`) or several
  shapes reuse it. Merge when two files differ only in task wording; a role that only picks another agent is a plugin
  leaf (`code-worker:<agent>`), not a file. Prefer few focused leaves: each costs a brief and a fan-in read.
- Prose instructs, hooks guarantee: a new type that requests external agents joins `ranOnGated`
  (`internal/hooks/agents/leaves.go`) and `labelTypes`/`labelPools` (`internal/hooks/agents/agenteffort.go`).

## Agent definition template

```markdown
---
name: <file-stem>          # house rule: equals agents/<name>.md stem
description: |
  When to use it, one paragraph. NOT ambiently triggered if it is a leaf.
model: sonnet              # alias or inherit; effort: low|medium|high|xhigh|max
effort: medium
tools: [Read, Grep, Glob]  # list them; a read-only role gets none that write
---

# <name>

## Role
## Invocation        (agents with a slash entry)
## Owns
## Knows             (on-demand references; nothing loads upfront)
## Verbs             (when it drives a CLI)
## Hands off to
## Procedure
## Report contract
## Don't
```

Also worth knowing: `maxTurns`, `isolation: worktree`, `background`, `memory`; unknown fields are silently ignored.

## Lessons from the trial

- Stream per package, no barrier between review and fix (#18, `06ac070e`).
- Record in-run, not in a hook: PostToolUse(Workflow) fires at launch (#23, `2e9a90ff`).
- Re-derive blind, and treat a failed re-derivation as unverified (`222ae2b9`).
- A fixer that is not `done` is red even when the gate passes (#33, `07f86477`).
- Route findings to the package that owns the file (#25, `d51420fb`).
- Scoped gates are not enough: full suite before every push (#24, #27).
- Revert a leaf's out-of-scope hunk (#20); a leaf never "fixes" files outside its list.
- Pin `model` and `effort` on every stage with one verbatim `stepOf` (#29, `222ae2b9`).
- Delete dead lanes: goal-keeper and `internal/goal` went (#8, `46da50b8`, `248e6db6`).
- Fix CRITICAL/HIGH/MEDIUM in the round, LOW to techdebt (#17).
