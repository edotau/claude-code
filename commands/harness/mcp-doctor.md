---
description: "Diagnose MCP server health, scope conflicts, and inline-credential hygiene"
argument-hint: "[health | conflicts | secrets | all]"
---

# /harness:mcp-doctor — MCP Diagnostic

`claude-code mcp doctor [--format json]` runs these checks natively; this command is the stock-CLI
walkthrough and inspects the config files directly. Pass the mode through as `$ARGUMENTS` (default `all` if empty).

## What each mode checks

| Mode        | How |
| ----------- | --- |
| `health`    | `claude mcp list` — which servers connect vs fail |
| `conflicts` | Read `~/.claude.json` (user scope) and any project `.mcp.json` for the SAME server name defined with **different commands** across scopes (a real conflict); same-command layering across scopes is benign |
| `secrets`   | Inspect `env` blocks of every MCP server entry in `~/.claude.json` and `.mcp.json` for inline plaintext credentials (should be `${ENV}` or a credential file) |
| `all`       | every check (default) |

## Execution

```bash
claude mcp list
cat ~/.claude.json 2>/dev/null
cat .mcp.json 2>/dev/null
```

Read the JSON with `Read`/`jq` rather than parsing by eye for anything beyond a quick scan.

After running, summarize the findings and — for any real conflict or inline
secret — propose the concrete fix (`claude mcp remove <name> -s <scope>`, or
move the token to `${ENV}` / `env.d/secrets.env`). Do **not** auto-remove or
rotate anything without confirming with the user first; this command is
read-only diagnosis.

Exit code: `0` clean, `1` findings to act on, `2` usage error.
