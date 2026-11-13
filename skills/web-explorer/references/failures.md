# Failures

| Symptom | Cause | Next step |
| --- | --- | --- |
| "key not set" / auth error / `Unauthorized` | Missing or wrong `TAVILY_API_KEY` | Stop; `claude-code vault set TAVILY_API_KEY`, check with `claude-code doctor`; never answer from memory |
| Network error or timeout | Transient | Retry once; if it fails again, report it |
| Empty or irrelevant search results | Weak query | Reformulate once (synonym, official name, drop a noisy term); still empty means "not found" |
| `403`, bot-protection page | Site blocks automated clients | Retry once with `tvly extract --extract-depth advanced`; then Chrome `get_page_text` (Claude); then report |
| `429` or rate-limit text | Too many calls to the site or to Tavily | Wait, cut the URL list, never loop-retry; keyless `tvly` is capped, so use the vault key |
| Empty extract or only a nav shell | JavaScript-only page | `--extract-depth advanced`, then Chrome (Claude) |
| Content cut near the needed fact | `--query` chunking | Re-extract without `--query` and read the saved file |
| Table content missing | Basic extraction | `--extract-depth advanced` |
| Login wall, paywall, 401 | Private content | Stop; use `gh` or an authenticated MCP, or ask the user |
| robots.txt disallows | Site opted out | Do not fetch around it; ask for an API or permission |
| Internal address refused | `localhost`, `192.168.x.x` | Use curl for a server you own |
| `tvly` exit code | 2 bad input, 3 auth, 4 API error | Fix the input; code 3 means the key |

- Chrome needs remote debugging: `curl -s http://127.0.0.1:9222/json/version` answers when it is available.
- Direct data links (CSV, JSON, PDF): `curl -sSL -o <file> <url>`. Never retry a block with spoofing, proxy rotation
  or CAPTCHA solving.
