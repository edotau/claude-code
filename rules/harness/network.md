---
paths:
  - "internal/network/**"
  - "internal/setup/**"
---
# Network, Web, MCP, Cleanup

| To… | Run |
| --- | --- |
| Search the web or read a page (any agent) | `claude-code network websearch --query "<query>"` · `--extract <url>` (`skills/web-explorer/SKILL.md`) |
| Check MCP health, scope drift, inline secrets | `claude-code doctor --mcp [--format json]` |
| Sweep accumulated state (caches, run dirs, backups) | `claude-code clean [--list\|--dry-run]` · opt-in `--target projections\|sessions` |
| Run any harness verb on a tailnet host | `claude-code --host <host> <verb …>` (ssh, `-t` on a terminal); `databricks login` forwards its callback port so the sign-in happens in your local browser, or `--host-browser` opens the host's (`rules/harness/providers.md` → Headless login) |

- Browser automation only on authorized sites; robots.txt is checked; no detection evasion
  (`rules/standards/security.md` → 5).
- `doctor --mcp` is read-only: propose the fix (`claude mcp remove <name> -s <scope>`, or move a token to `${ENV}`
  via `vault set`); never remove or rotate without the user's confirmation.
- `clean` applies by default (`--dry-run` previews) and never sweeps credentials or settings.
