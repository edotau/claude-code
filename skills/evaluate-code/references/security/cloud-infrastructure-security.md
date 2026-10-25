# Cloud & Infrastructure Security

What to check when a change touches cloud config, CI/CD or infrastructure-as-code. The vendor docs hold the
how; this is the list a review must cover.

- **IAM**: least privilege per role; no wildcard actions or resources; no long-lived user keys where a role works.
- **Humans and break-glass**: MFA on every human account; break-glass credentials sealed and audited.
- **Secrets**: never in CI env dumps, logs, images or IaC state; sourced from a secrets manager (here: the vault,
  `claude-code vault set`).
- **Network exposure**: no public ingress by default; security groups and WAF rules reviewed like code.
- **Logging and audit**: control-plane audit logs on and retained; alerts on privilege changes.
- **Backups**: taken, encrypted, and restore-tested, with a stated RPO/RTO.
- **IaC and pipelines**: plans reviewed before apply; pipeline tokens scoped to one repo and one environment;
  production applies only through the deploy pipeline (`rules/standards/security.md` principle 5).
