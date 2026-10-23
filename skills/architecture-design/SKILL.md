---
name: architecture-design
description: 'Use when designing or assessing system architecture — "design the architecture", "monolith vs microservices", "which database/queue/cache", "make a technical decision", "review the system design", "architecture diagram", "analyze dependencies", "plan a migration", or writing an ADR. For code-level quality see evaluate-code; for security see security-reviewer.'
---

# Architecture Design

Make and document architecture decisions, and assess what already exists. This is the
**design-altitude** companion to the `claude` agent's Architect phase — `claude`
drives it when a coding task carries real design choices; use the skill directly when the
question is purely architectural (which datastore, which pattern, how to migrate).

**Stack-native first.** Prefer the option the project already runs over the generically
"best" one; a new datastore, broker, or pattern is a cost someone must own (`references/` say
when it's justified). Establish the current stack before deciding — read the manifests,
deploy config and existing services, or run `project_architect.py`.

**Local overlay.** If `references/local/` exists, it pins a specific team's stack and its
defaults override the generic ones below — read it first. It is gitignored and never published.

## Scripts (analysis tools)

Stdlib-only; resolve the dir from the project, falling back to global:

```bash
AD="${CLAUDE_PROJECT_DIR:-$HOME}/.claude/skills/architecture-design/scripts"
[ -d "$AD" ] || AD="$HOME/.claude/skills/architecture-design/scripts"

# Assess an existing codebase — detected pattern, layer violations, god files
python3 "$AD/project_architect.py" ./path --verbose

# Dependency tree, coupling score, circular deps (npm/pip/poetry/go/cargo)
python3 "$AD/dependency_analyzer.py" ./path --output json

# Generate an architecture diagram (mermaid | plantuml | ascii)
python3 "$AD/architecture_diagram_generator.py" ./path --format mermaid --type component
```

All three are advisory: they **locate** structure and smells; you judge. Run any with
`--help` for flags. Use their output as leads for the workflows below — don't forward raw
JSON into a report. Scripts are MIT-licensed upstream code (`scripts/LICENSE`).

## Decision workflows

Pick the workflow that matches the task; full step lists are in the references.

| Task | Reference | Lands in |
|------|-----------|----------|
| New pipeline stage | `references/system-design-workflows.md` §1 | contract + verify plan |
| New web feature (API + UI) | `references/system-design-workflows.md` §2 | layered impl plan |
| Compute placement (data platform vs app platform) | `references/system-design-workflows.md` §3 + `tech-decision-guide.md` §6 | placement decision |
| Capacity / scalability check | `references/system-design-workflows.md` §4 | bottleneck list |
| Migration (expand→migrate→contract) | `references/system-design-workflows.md` §5 | phased, reversible plan |
| Pick a datastore / cache / async model | `references/tech-decision-guide.md` | choice + rationale |
| Choose / resist a pattern | `references/architecture-patterns.md` | pattern + trade-offs |

**Output an ADR** when the decision is hard to reverse, sets precedent, or reaches outside the
stack-native defaults — template in `references/system-design-workflows.md` §6.

## Delivery decisions

This skill makes the **decision**; it doesn't author CI/CD pipelines, manifests or IaC — hand
those to a delivery/devops skill if one is installed.

- *Where does it run?* — placement test (`tech-decision-guide.md` §6).
- *How is async handled?* — the scheduler/task queue you already run before a broker (`tech-decision-guide.md` §3).
- *How does it roll out?* — expand→migrate→contract, reversible per step (workflows §5).

## References

- `references/architecture-patterns.md` — modular monolith, layered, staged pipeline,
  hexagonal, job-driven async + patterns to **resist** and why.
- `references/tech-decision-guide.md` — datastore / cache / async / auth / API / compute-
  placement matrices.
- `references/system-design-workflows.md` — step-by-step workflows + the ADR template.

## Boundaries

- **Code-level quality** (complexity, surgical changes, bug-hunting) → `evaluate-code` skill /
  `claude` agent.
- **Security** (auth/crypto/SSRF, secret handling) → `security-reviewer`.
- **Performance profiling / hot-path fixes** → `evaluate-code` skill's performance lens (`references/performance.md`).
