# Dependency Auditor

Audit a project's dependencies for **known vulnerabilities (CVEs)**, **license compliance**,
and **safe upgrade planning**. This is Mode 3 of the security-reviewer skill.

> **Use live-feed scanners, never a hardcoded CVE list.** Vulnerability data goes stale in
> days — a scanner matching against a baked-in list reports "clean" on a project full of real
> CVEs. Every command below queries a maintained advisory database (OSV, PyPI, npm registry) at
> run time. If none is installable (air-gapped), say so and report *unknown*, never *clean*.

## When to use

- "audit dependencies", "check for vulnerable packages", "any CVEs in our deps?"
- Before a release, or when adding/bumping a dependency (auditing the dep here; whether to add
  a new dependency at all — prefer a maintained library over custom code — is a design-time call).
- License-compliance questions ("can we ship with this GPL package?").
- Planning a batch of dependency upgrades safely.

## Vulnerability scan — pick the tool for the ecosystem

Prefer `osv-scanner` (Google OSV, all ecosystems, one tool) when available; fall back to the
per-ecosystem native tool. All hit live databases.

| Ecosystem | Command | DB |
|-----------|---------|-----|
| Any (lockfiles) | `osv-scanner scan --lockfile=<path>` or `osv-scanner scan .` | OSV.dev |
| Python | `pip-audit -r requirements.txt` (or `pip-audit` in an active venv) | PyPI Advisory + OSV |
| Node | `npm audit --json` (needs `package-lock.json`) | GitHub Advisory |
| Containers/fs | `trivy fs .` / `trivy image <ref>` | Aqua/OSV/vendor feeds |

Install on demand (none are guaranteed present): `pipx install pip-audit osv-scanner` or
`brew install osv-scanner trivy`. **`npm` is the only scanner present by default here** — probe
with `command -v <tool>` first and install or state the gap; do not silently skip.

Severity mapping into the skill's grades: CVSS ≥ 9.0 or known-exploited → **CRITICAL**;
7.0–8.9 → **HIGH**; 4.0–6.9 → **MEDIUM**; < 4.0 → **LOW**. A fixed version being available
raises urgency (the fix is one bump away); no-fix-yet lowers actionability, not severity.

```bash
# Python project, JSON for parsing, fail CI on anything fixable
pip-audit -r requirements.txt --format=json --fix --dry-run

# Whole repo, all ecosystems, one pass
osv-scanner scan --recursive . --format=json
```

Report each finding as: package, installed version, CVE/GHSA id, severity, **fixed version**
(the actionable field), and the dependency path (direct vs transitive — transitive means the fix
is a parent bump, not a direct edit).

## License compliance

Enumerate installed licenses, then check each against the project's own license using the
compatibility matrix in `dependency-license-matrix.md`.

```bash
pip-licenses --format=json --with-urls            # Python (pipx install pip-licenses)
npx license-checker --json                         # Node
osv-scanner scan --experimental-licenses . 2>/dev/null || true   # if the OSV build supports it
```

The **project's outbound license constrains what it may consume**: an MIT/Apache/BSD project can
consume permissive + weak-copyleft (LGPL/MPL) but **not** GPL/AGPL as a linked dependency. AGPL is
the sharpest edge — it reaches SaaS/network use, so a hosted service pulling an AGPL lib inherits
AGPL obligations. Flag GPL/AGPL in a permissively-licensed or proprietary codebase as **HIGH**;
LGPL/MPL as **MEDIUM** (needs dynamic-linking / file-level review). See the matrix for the full grid.

## Upgrade planning

For a batch of outdated or vulnerable deps, sequence upgrades by **risk × benefit** rather than
bumping everything at once:

1. **Inventory** current vs latest: `pip list --outdated --format=json`, `npm outdated --json`.
2. **Classify each bump** by semver distance: patch (low risk) < minor (medium) < major (high —
   read the changelog for breaking changes).
3. **Prioritize**: security-fix patches first (highest benefit, lowest risk) → other patches →
   minors → majors last, one at a time with tests between.
4. **Verify each step** — run the project's test + lint gate after every bump; a green suite is
   the gate to the next upgrade. Never batch a major with anything else.

Security-only, fastest path: `pip-audit --fix` / `npm audit fix` applies just the
vulnerability-closing bumps (review the diff — `--force`-style majors can break you).

## Honest reporting rules

- **Scanner absent ≠ clean.** If no live scanner is installable, report the packages you found
  and mark vulnerability status **UNKNOWN**, with the exact command to run when a scanner is available.
- **A scan is a point-in-time snapshot.** State the DB and date; a re-scan next week may differ.
- **Transitive fixes** may have no direct action (waiting on an upstream release) — say so rather
  than inventing a bump.

## Quick reference

| Task | Command | Notes |
|------|---------|-------|
| All-ecosystem vuln scan | `osv-scanner scan --recursive .` | best single tool |
| Python vuln scan | `pip-audit -r requirements.txt` | live PyPI+OSV |
| Node vuln scan | `npm audit --json` | needs lockfile |
| Container/fs scan | `trivy fs .` | OSV + vendor |
| Python licenses | `pip-licenses --format=json` | + check matrix |
| Outdated (py/node) | `pip list --outdated` / `npm outdated` | upgrade inputs |
| Auto-fix security | `pip-audit --fix` / `npm audit fix` | review diff |

## Common mistakes

- **Trusting a static/bundled CVE list** — always query a live DB; stale lists miss real CVEs.
- **Reporting "no vulnerabilities" when the scanner wasn't installed** — that's *unknown*, not clean.
- **Scanning manifests, not lockfiles** — `requirements.txt` ranges / `package.json` hide the
  resolved transitive versions where most CVEs live. Scan the lockfile.
- **Bumping a major to fix a low-severity transitive CVE** — check whether a patch/minor closes it first.
- **Ignoring license direction** — a compatible pair one way (project→dep) can be incompatible the other.
