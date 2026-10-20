---
description: "Audit the Claude config surface for plaintext credentials that belong in the vault (`claude-code secrets set`) or a credential file"
argument-hint: "[scan | git | all]"
---

# /harness:secrets-scan — Credential Hygiene Audit

`claude-code` has no dedicated secrets-scan verb; this command is a thin wrapper over the
**security-reviewer** skill (`/security`), scoped to the harness's own config surface instead
of application code. Pass the mode through as `$ARGUMENTS` (default `scan`).

## What each mode checks

| Mode | Checks |
|------|--------|
| `scan` | Every leaf string in `~/.claude.json`, `settings.json`, `settings.local.json`, the vault's provider overlay (`claude-code vault show`), and `.mcp.json` (user + project + cwd) for credential-shaped values that are NOT `${ENV}` refs, file paths, or placeholders |
| `git` | `git grep` the current repo's **tracked** files for secret-shaped strings (API keys, tokens, bearer secrets). Reports `path:line`, never the value |
| `all` | both |

## Execution

Run `Skill(security-reviewer)` over the file set above with the secret-detection checklist
from `rules/standards/security.md` (no hardcoded secrets, `.env`/credential files excluded
from git). For `git` mode:

```bash
git grep -nIE '(api[_-]?key|secret|token|bearer)[[:space:]]*[:=][[:space:]]*["'"'"'][A-Za-z0-9_\-]{16,}' -- . || true
```

Severity: a finding in a **git-tracked** file is marked `TRACKED` (urgent — it would be
committed or already is); a finding in `settings.local.json` or the vault overlay (both
untracked) is `local`. Treat `${VAR}`, absolute/`~/` paths, and `PLACEHOLDER`/`YOUR_*`/`xxx`
stubs as safe.

After running, summarize findings. For each real secret, recommend: (1) rotate the
credential at its source, (2) replace the inline value with `${ENV_VAR}` (resolved via
the vault — `claude-code secrets set` — or `auth.command`), (3) for `TRACKED` hits, scrub history if already
pushed. Do **not** edit files or rotate anything automatically — confirm with the user first.

Pairs with `/harness:mcp-doctor` (MCP-scoped).
Exit code: `0` clean, `1` secret(s) found, `2` usage error.
