---
name: web-search
description: Search the public web for current/real-time information when the built-in WebSearch is unavailable (e.g. on a non-Anthropic provider). Trigger on "search the web", "look this up online", "what's the latest", "current/recent news", "find recent info", or any question needing facts beyond training data. Backed by Tavily via `claude-code network websearch`.
allowed-tools: Bash, mcp__web_search__web_search
---

# Web Search

Restores web search on providers where the built-in `WebSearch` tool is unavailable. Backed by Tavily
through `claude-code network websearch` (`internal/websearch`). Two ways to call it — pick by what the agent
already has:

## Which path

| You have | Use | How |
|---|---|---|
| The MCP tool (`mcp__web_search__web_search`) | **Prefer this** — structured, least-privilege | Call the tool with `{query, max_results?}` |
| Only `Bash` (e.g. gemini, gemini-driving agents) | One-shot CLI | `claude-code network websearch --query "<query>" [--max-results N]` |

Both hit the same Tavily backend. The CLI prints an answer + ranked results to stdout and exits;
the MCP tool returns the same text as a tool result.

## Examples

```bash
# One-shot from any agent with Bash:
claude-code network websearch --query "Databricks Asset Bundles latest schema changes"
claude-code network websearch --query "OpenShift 4.16 release notes" --max-results 3
```

MCP tool call (interactive sessions / agents granted the tool):
`web_search({"query": "who founded Databricks", "max_results": 5})`

## When to use / not

- **Use** for current events, recent releases, live facts, anything past the training cutoff, or
  when the user explicitly asks to search online.
- **Don't** use for fetching a *known* URL — that's `WebFetch`. Web search discovers pages;
  `WebFetch` reads one you already have.
- **Don't** use for internal/GitHub lookups — use `gh` or the repo tools.

## Setup dependency

Requires `TAVILY_API_KEY` via `claude-code vault set TAVILY_API_KEY` (OS keyring; mint free at
app.tavily.com → API keys). Without it the tool returns a friendly "key not set" message.
