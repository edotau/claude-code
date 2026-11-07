# Fetch failures

| Symptom | Cause | Next step |
| --- | --- | --- |
| `403`, bot-protection page | Site blocks automated clients | Retry once at `--extract-depth advanced`; then Chrome `get_page_text`; then report |
| `429` or rate-limit text | Too many calls to the site or to Tavily | Wait, cut the URL list, never loop-retry; keyless `tvly extract` is capped, so use the vault key |
| Empty extract or only a nav shell | JavaScript-only page | `--extract-depth advanced`, then Chrome |
| Content cut near the needed fact | `--query` chunking | Re-extract without `--query` and read the saved file |
| `Unauthorized` from map/crawl | Missing or wrong key in `TAVILY_API_KEY` | `claude-code vault set TAVILY_API_KEY`; check with `claude-code doctor` |
| Login wall, paywall, 401 | Private content | Stop; use `gh` or an authenticated MCP, or ask the user |
| robots.txt disallows | Site opted out | Do not fetch around it; ask for an API or permission |

Chrome needs remote debugging: `curl -s http://127.0.0.1:9222/json/version` answers when it is available.

Direct data links (CSV, JSON, PDF) are better downloaded with `curl -sSL -o <scratch file> <url>` than extracted.
