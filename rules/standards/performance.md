# Performance Optimization

## Model Selection Strategy

Reference the logical **tier** (`opus`/`sonnet`/`haiku`/`fable`), not a concrete model id — `providers.json`
maps each provider's tier slot to an id, so this doc doesn't rot when a model moves.

- **`opus`** — deepest reasoning: architectural decisions, maximum-reasoning research and analysis.
- **`sonnet`** — best all-round coding: planning, code review, orchestration, codebase exploration (the
  `gemini` agent's in-process backend), default for `code-worker` fan-out leaves.
- **`haiku`** — high-frequency, cost-sensitive: lightweight agents, worker leaves, quick lookups.
- **Gemini** (`claude-code gemini bridge`) — the large-sweep backend when whole-repo context exceeds a session
  window (1M tokens across many inlined files in one call).

Pin a slot durably with `claude-code models --pin opus=<id>` (or `--pin all=<id>`); cross a provider for one
slot with `provider:model` (e.g. `--pin haiku=openai:gpt-5-nano`). `claude-code models --live` shows what a
provider actually serves before pinning.

## Context Window Management

Avoid the last 20% of the window for large-scale refactoring, multi-file features, and complex debugging.
Single-file edits, utilities, docs, and simple fixes are context-cheap.

Agent concurrency caps, extended-thinking and prompt-caching guidance for authoring agents and provider config:
`rules/workflow/agents.md`.
