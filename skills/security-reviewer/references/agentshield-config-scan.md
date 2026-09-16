# Mode 2: Configuration Security Scan (AgentShield)

Scan Claude Code configuration files for security issues.

## What Gets Scanned

| Target | Checks |
|--------|--------|
| CLAUDE.md | Prompt injection vectors, overly permissive instructions |
| settings.json | Dangerous permissions, exposed secrets in env vars |
| MCP configs | Untrusted servers, excessive permissions |
| Hooks | Command injection in hook scripts, unsafe paths |
| Agents | Overly broad tool access, missing constraints |

## Scan Commands

```bash
# Basic scan
npx ecc-agentshield scan

# Auto-fix safe issues
npx ecc-agentshield scan --fix

# Adversarial 3-agent pipeline (thorough)
npx ecc-agentshield scan --opus
```

## Severity Grades

| Grade | Meaning |
|-------|---------|
| A | Excellent — no issues found |
| B | Good — minor suggestions |
| C | Fair — some issues to address |
| D | Poor — significant security gaps |
| F | Critical — immediate action required |

## Common Config Issues

- `Bash(*)` permission without scoping (use `Bash(git:*)` instead)
- Secrets in `settings.json` env vars (use `.env` files)
- MCP servers from untrusted sources
- Hook scripts without input validation
- Agents with unrestricted tool access

For cloud / IaC / IAM / CI-CD security review (Terraform, K8s, cloud config), see
`cloud-infrastructure-security.md` in this skill directory.
