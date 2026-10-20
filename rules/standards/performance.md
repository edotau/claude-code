# Performance Optimization

## Model Selection Strategy

Reference the logical **tier** (`opus`/`sonnet`/`haiku`/`fable`), not a concrete model id —
`providers.json` maps each provider's tier slot to a model id, so this doc doesn't rot when a
model moves.

**`opus`** — deepest reasoning:
- Complex architectural decisions and implementation
- Maximum-reasoning + research/analysis tasks

**`sonnet`** — best all-round coding:
- Planning, code review, orchestrating multi-agent workflows
- Codebase exploration (the `gemini` agent's in-process backend)
- Default for `code-worker` fan-out leaves

**`haiku`** — high-frequency, cost-sensitive:
- Lightweight, frequently-invoked agents; worker/leaf agents in fan-outs
- Straightforward code generation, quick lookups

**Gemini** (via `claude-code gemini bridge`) — the large-sweep backend for whole-repo context
that exceeds a single session window (1M tokens across many inlined files in one call).

Pin a slot durably with `claude-code models --pin opus=<id>` (or `--pin all=<id>`); cross a
provider for one slot with `provider:model` (e.g. `--pin haiku=openai:gpt-5-nano`). See
`<id> models --live` to check what a provider actually serves before pinning.

## Context Window Management

Avoid the last 20% of the window for large-scale refactoring, multi-file features, and
complex debugging. Single-file edits, utilities, docs, and simple fixes are context-cheap.

## Extended Thinking

Current Claude models run thinking as **adaptive** — the SDK omits a fixed `budget_tokens`
unless the specific model requires one. Don't hand-roll a thinking-budget override in a skill
or command; if a model genuinely needs a fixed budget, that belongs in provider config, not in
prose instructions repeated per task.

## Prompt Caching

For repeated large prefixes (system prompts, CLAUDE.md, big tool schemas), mark the stable
prefix with `cache_control` so re-sends bill at the cached rate. Keep the design-system
invariant: content that changes every call must NOT sit inside a cached block, or the cache
either never hits or silently serves stale context.

## Agent Concurrency

**Cap concurrency by context weight, not raw count** (`rules/workflow/agents.md`): deep-context
agents (large context / many files each) stay ≤8 concurrent; lean/fresh subagents (one-file
reads, single-target fixes) can run ~8–10 at once. A provider's own rate limit, if it has one,
is the practical ceiling beyond that — `claude-code doctor` reports the router/fallback state
before you assume more headroom exists than there is.

## Build Troubleshooting

If a build fails: dispatch `codex` (VERIFY mode, via `claude-code ask --agent codex`) in a
build/lint/test/security gate loop, read the error, fix incrementally, re-verify after each fix.
