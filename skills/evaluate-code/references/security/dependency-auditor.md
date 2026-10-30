# Dependency Auditor

The Dependencies mode of `evaluate-code`'s Security lens: known CVEs, licences, safe upgrades.

**Live-feed scanners only, never a hardcoded CVE list** — baked-in data goes stale in days. Probe with
`command -v <tool>`; a scanner that isn't installed reports **UNKNOWN**, never "clean". Read-only: no `--fix`.

| Ecosystem | Command | Here |
| --- | --- | --- |
| Go | `govulncheck ./...` | installed; `make audit` runs it |
| Any lockfile | `osv-scanner scan --recursive .` | `pipx install osv-scanner` on demand |
| Python | `pip-audit -r requirements.txt` | `pipx install pip-audit` on demand |
| Node | `npm audit --json` (needs `package-lock.json`) | with npm |

Severity: CVSS ≥ 9.0 or known-exploited → CRITICAL; 7.0–8.9 → HIGH; 4.0–6.9 → MEDIUM; < 4.0 → LOW. Report each
finding as package, installed version, CVE/GHSA id, severity, **fixed version**, and direct vs transitive (a
transitive fix is a parent bump).

Licences: `pip-licenses --format=json` (Python), `npx license-checker --json` (Node); check each against the
project's licence with `dependency-license-matrix.md`.
