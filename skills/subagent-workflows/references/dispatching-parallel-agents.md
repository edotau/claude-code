# Dispatching Parallel Agents

For two or more tasks with no dependency, shared state or overlapping writes — not when one fix may resolve the
others or the agents need the same evolving context. Rules: [SKILL.md](../SKILL.md).

- **Same question, many angles** (research, model comparison, a wide sweep): one `claude-code dispatch
  explore|compare|fleet` call fans out up to 12 legs and returns one report — no hand-split leaves.
- **Code changes**: partition by file ownership, not topic. In one package each lane creates its own new file and edits
  nothing existing; the manager makes the one integration edit after both report, from the API surface each returned.

```text
LANE 1  -> internal/proxy/tokencache.go   (new, owned)
LANE 2  -> internal/proxy/authretry.go    (new, owned)
MANAGER -> internal/proxy/proxy.go        (integration, one writer, after both report)
```

Process: one brief per lane (contract in SKILL.md), all `Agent` calls in one message; integrate, simplify the merged
diff, run the fan-in checks. A brief's `Agent: <name>` line routes the leaf and labels it
(`code-worker  codex: <task>`, [SKILL.md → Leaf types](../SKILL.md#leaf-types)).
