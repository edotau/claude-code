# Performance Optimization

## Model selection

Name the alias (`opus`/`sonnet`/`haiku`/`fable`), never a concrete model id — `internal/providers/defaults.json`
plus the vault overlay map each alias per provider.

- **`opus`** — architectural decisions, maximum-reasoning research and analysis.
- **`sonnet`** — planning, review, orchestration, exploration; the default `code-worker` leaf.
- **`haiku`** — high-frequency, cost-sensitive leaves and quick lookups.
- **Gemini** (`claude-code gemini bridge`) — sweeps whose context exceeds a session window (1M tokens).

## Context window

Avoid the last 20% of the window for multi-file features, large refactors and complex debugging; single-file edits,
docs and simple fixes are cheap. Concurrency caps, thinking and caching for agent authors: `rules/workflow/agents.md`.
