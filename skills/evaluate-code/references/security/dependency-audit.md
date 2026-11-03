# Dependency audit — CVEs and licences

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

## Licences

List with `pip-licenses --format=json` (Python) or `npx license-checker --json` (Node), then check each against the
project's licence below. **Guidance, not legal advice** — escalate GPL/AGPL-in-proprietary questions to whoever
owns licensing in your organization.

- **Permissive** — MIT, Apache-2.0, BSD-2/3, ISC: usable anywhere with attribution.
- **Weak copyleft** — LGPL-2.1/3.0 (linking allowed, library changes shared), MPL-2.0 (file-level).
- **Strong copyleft** — GPL-2.0/3.0 (the whole derivative work), AGPL-3.0 (extends to network services).

Can a project under the row licence use a dependency under the column licence? ✅ yes · ⚠️ with conditions
(LGPL by dynamic linking, MPL files stay MPL) · ❌ no.

| Project | MIT | Apache-2.0 | BSD-3 | LGPL-2.1 | LGPL-3.0 | MPL-2.0 | GPL-2.0 | GPL-3.0 | AGPL-3.0 |
|---|---|---|---|---|---|---|---|---|---|
| **MIT** | ✅ | ✅ | ✅ | ⚠️ | ⚠️ | ⚠️ | ❌ | ❌ | ❌ |
| **Apache-2.0** | ✅ | ✅ | ✅ | ❌ | ⚠️ | ✅ | ❌ | ⚠️ | ⚠️ |
| **BSD-3** | ✅ | ✅ | ✅ | ⚠️ | ⚠️ | ⚠️ | ❌ | ❌ | ❌ |
| **LGPL-2.1** | ✅ | ❌ | ✅ | ✅ | ❌ | ❌ | ✅ | ❌ | ❌ |
| **LGPL-3.0** | ✅ | ✅ | ✅ | ❌ | ✅ | ✅ | ❌ | ✅ | ✅ |
| **MPL-2.0** | ✅ | ✅ | ✅ | ❌ | ✅ | ✅ | ❌ | ✅ | ✅ |
| **GPL-2.0** | ✅ | ❌ | ✅ | ✅ | ❌ | ❌ | ✅ | ❌ | ❌ |
| **GPL-3.0** | ✅ | ✅ | ✅ | ❌ | ✅ | ✅ | ❌ | ✅ | ✅ |
| **AGPL-3.0** | ✅ | ✅ | ✅ | ❌ | ✅ | ✅ | ❌ | ✅ | ✅ |

Why the odd cells: GPL-2.0 is incompatible with Apache-2.0 (patent terms) and with any `-3.0` license (version
mismatch); GPL-3.0 and LGPL-3.0 carry an Apache compatibility clause.

Fixes for a copyleft dependency in a permissive or proprietary project (a GPL/AGPL one pulls the whole project
under it): find a non-copyleft alternative, call it across a service/API boundary, obtain a commercial licence,
change the project licence, or drop the dependency. For GPL-2.0 under Apache, upgrade the dependency to GPL-3.0
if offered.
