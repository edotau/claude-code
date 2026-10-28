# Performance Optimization

## Model Selection Strategy

Reference the model alias (`opus`/`sonnet`/`haiku`/`fable`), not a concrete model id —
`internal/providers/defaults.json` (plus the vault overlay) maps each provider's alias to a model, so this doc
doesn't rot when a model moves.

- **`opus`** — deepest reasoning: architectural decisions, maximum-reasoning research and analysis.
- **`sonnet`** — best all-round coding: planning, code review, orchestration, codebase exploration (the
  `gemini` agent's in-process backend), default for `code-worker` fan-out leaves.
- **`haiku`** — high-frequency, cost-sensitive: lightweight agents, worker leaves, quick lookups.
- **Gemini** (`claude-code gemini bridge`) — the large-sweep backend when whole-repo context exceeds a session
  window (1M tokens across many inlined files in one call).

Pin a model alias durably with `claude-code models --pin opus=<id>` (or `--pin all=<id>`). `claude-code models --live`
shows what a profile actually serves before pinning.

## Context Window Management

Avoid the last 20% of the window for large-scale refactoring, multi-file features, and complex debugging.
Single-file edits, utilities, docs, and simple fixes are context-cheap.

Agent concurrency caps, extended-thinking and prompt-caching guidance for authoring agents and provider config:
`rules/workflow/agents.md`.
