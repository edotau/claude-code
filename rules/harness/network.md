---
paths:
  - "internal/network/**"
  - "internal/clean/**"
---
# Network, Web, MCP, Cleanup

| To… | Run |
| --- | --- |
| Web search when native search is unavailable | `claude-code network websearch --query "<query>"` (`skills/web-search/SKILL.md`) |
| Scrape repeated records | `claude-code network browser --extract <url> --container <sel> --field name=sel[@attr]` (`--next\|--pages\|--scroll\|--load-more`, `--format jsonl\|csv\|json`) |
| Check MCP health, scope drift, inline secrets | `claude-code network mcp [--format json]` |
| Sweep accumulated state (caches, run dirs, backups) | `claude-code clean [--list\|--dry-run]` · opt-in `--target projections\|sessions` |
| Map the whole harness | `claude-code describe [--format json\|text]` |

- Browser automation only on authorized sites; robots.txt is checked; no detection evasion
  (`rules/standards/security.md` → 5).
- `network mcp` is read-only: propose the fix (`claude mcp remove <name> -s <scope>`, or move a token to `${ENV}`
  via `vault set`); never remove or rotate without the user's confirmation.
- `clean` applies by default (`--dry-run` previews) and never sweeps credentials or settings.
