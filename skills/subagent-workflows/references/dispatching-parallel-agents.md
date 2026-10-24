# Dispatching Parallel Agents

For two or more tasks with no dependency, shared state or overlapping writes. Not when one fix may resolve the
others, or the agents need the same evolving context. Dispatch, shared-tree and fan-in rules: [SKILL.md](../SKILL.md).

## Same package, disjoint new files

"Same package" and "same file" are different problems. Split package work so each agent creates its own new file
and may not edit existing ones; the manager makes the one integration edit afterwards.

```text
LANE 1  -> internal/router/tokencache.go   (new, owned)
LANE 2  -> internal/router/authretry.go    (new, owned)
MANAGER -> internal/router/router.go       (integration, one writer, after both report)
```

Ask each lane to report the exact API surface it produced, so integration needs no re-reading.

## Process

1. Partition by file ownership, not topic.
2. One brief per agent (contract in SKILL.md), all `Agent` calls plus `copilot` in one message.
3. When an implementer reports, send `copilot` the "landed" message with files and signatures.
4. Integrate, simplify the integrated diff, run the fan-in checks.

## Example brief

```text
Agent(subagent_type="code-worker", description="Token cache for the router", prompt="""
Own ONLY: internal/router/tokencache.go (new). Do NOT edit router.go — other agents are in this package.
Objective: in-memory, provider-keyed credential cache with single-flight and a refresher.
Constraints: keyed by provider name, never process-global; providers/creds.go's TTL stays authoritative;
  never log a credential. Tests belong to copilot: list the behaviors that need coverage.
Verify: gofmt -l <files> && go build ./... && go vet ./internal/router
Shared tree: no stash/checkout/reset/restore; confirm each write with git diff --stat.
Return: files, exact API surface to wire, verification output, risks.
""")
```
