# Dispatch Patterns

Serves `../SKILL.md` Phase 0: pick the shape here, then fill each node with a leaf type (`../SKILL.md` → Leaf types)
and an agent ([cost-routing.md](cost-routing.md)). Shape and agent are separate choices: any node can be `code-worker`
(Claude does it) or a requested agent.

The harness runs subagent mode only: leaves are terminal, the main session is the one orchestration layer, a saved
Workflow nests one level (the Workflow runtime's rule for `workflow()`), and peer teams (`TeamCreate`) are not used:
agent teams are experimental, one per session, with no nesting. Subagents may themselves spawn subagents to depth 3
(`CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH`); the harness keeps leaves terminal. Each pattern below is mapped onto that.

Start simple: use the simplest shape that works and add structure only when it measurably helps
([Building effective agents](https://www.anthropic.com/engineering/building-effective-agents)).

| Pattern | Harness shape | Fits | Branches decided by | Enforced by |
| --- | --- | --- | --- | --- |
| Pipeline | [Sequential plan](#sequential-plan) · `code-engineer` | each step feeds the next | plan checklist (model) · `workflows/*.js` (code) | `docs-plan-tick` ticks lines from commits |
| Fan-out / fan-in | parallel leaves in one message · `agents dispatch explore\|fleet` | independent work on disjoint files | manager (model) | `dispatch-effort` cap · `worktree-claim-check` file ownership |
| Expert pool | one leaf, type or `Agent:` picked per task | input type picks the specialist | `gateway router` ladder and gates (code) | `dispatch-effort` routing and quota offload |
| Producer–reviewer | [Sequential plan](#sequential-plan) review loops · `green-keeper-loop` · `codex review` | objective pass criteria | verdict word or gate exit code (code) | loop caps: 3 per test, 5 per task |
| Supervisor / hierarchical | [Lane orchestrator](lane-orchestrator.md), 2 levels max | many leaf-sized tasks, assigned as they finish | manager → lane (model) | lane checklist; `ask --batch` per-account caps |
| Tournament | [Tournament](#tournament) | a measurable goal or truly diverging approaches | the eval number (code), then a judge | worktree isolation per contestant |

**Combine** by stage, not by mixing: Pipeline + fan-out = a plan whose one task fans out; fan-out + producer–reviewer =
parallel implementers, each followed by its own review. Never add a third level: a lane leaf does not orchestrate.

**Prefer the narrower shape.** A multi-agent run costs about 15x the tokens of a chat, and coding has fewer truly
parallel tasks than research ([multi-agent research system](https://www.anthropic.com/engineering/multi-agent-research-system)). One leaf beats a fan-out when the parts share files; a fixed pipeline beats a lane when
the task list is known up front; a router pick beats a compare when one answer is enough.

**Who tracks what** — one owner per kind of state, no side files: task state = the plan checklist (`plan status`), run
state = dispatch records (`agents tree`, `agents status|tail <id>`), proof an external agent ran = its `ask: ran on`
line (`dispatch-stop` gate), work product = git plus `$(claude-code memory path)/reports/`.

## Sequential plan

Per task: implementer → spec review until `SPEC_COMPLIANT` → `Skill(simplify)` → quality review until `SHIP` → run its
verification yourself ([prompts.md](prompts.md)). Stop and ask when an assumption breaks, scope grows, or a decision
falls outside the plan.

## Tournament

For a measurable goal (latency, size, pass rate) or truly diverging approaches worth N× the cost, within the cap:

- Baseline the eval; send one brief verbatim to N `code-worker`s (`isolation: "worktree"`, branch `compete/<slug>/<n>`,
  one named constraint varied). Answers only: `dispatch compare --models a,b`.
- Re-measure yourself, discard red, rank on the diff: Metric (eval vs baseline) · Judge (correctness → simplicity →
  quality) · Hybrid (metric, then judge leaves within ~10%). `claude-code worktree merge` the winner, re-run the gates,
  `git tag compete/<slug>/<n>` the losers. All red → merge nothing; none beats baseline (a tie is not a win) → keep it.
