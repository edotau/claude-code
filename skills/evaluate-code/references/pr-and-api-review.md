# PR & Contract Review — Blast Radius and Breaking Changes

Depth reference for the SKILL's "PR & contract review" section. Two judgment passes the
stdlib detectors can't make for you: **what else could this change break** (blast radius)
and **does it break a published contract** (breaking changes). Examples use Django/DRF, a data
warehouse, and React; substitute your stack's equivalents. The diff-fetch commands assume git; for
GitHub PRs use `gh pr diff`.

## Scope the diff first

```bash
BASE=$(git merge-base HEAD main)
git diff "$BASE"...HEAD --stat            # what moved, how much
git diff "$BASE"...HEAD --name-only       # bare file list for the passes below
# GitHub PR: gh pr diff <id> > /tmp/pr.diff
```

A PR over ~200 changed lines, or one touching a shared module / DRF serializer / migration
/ shared data table, gets the full treatment below. A localized leaf change does not.

## Pass 1 — Blast radius

For each changed file, ask *who depends on this* before judging the change in isolation. A
5-line edit to a shared helper can break every pipeline stage that imports it.

```bash
# Python importers of a changed module
grep -rn "from mypkg.pipelines import\|import mypkg.pipelines" --include="*.py" .

# Go importers of a changed package
grep -rn '"example.com/mymod/internal/pipelines"' --include="*.go" .

# DRF: who renders a changed serializer / hits a changed view
grep -rn "MySerializer\|MyViewSet" --include="*.py" src/

# React/TS: importers of a changed component or hook
grep -rn "from ['\"].*useBindingData['\"]" --include="*.ts" --include="*.tsx" src/

# Data: references to a table/path whose schema or location changed
grep -rn "schema.my_table\|/data/my_dataset" --include="*.py" --include="*.sql" .
```

Severity of the surface touched — set the review bar from the *most* exposed file:

| Blast radius | Surface |
|---|---|
| CRITICAL | public API contract, DB model/migration, auth/token plumbing, a table other jobs read, a shared pipeline stage |
| HIGH | a module imported by 3+ others, shared settings/env var, a job other jobs depend on |
| MEDIUM | single-module internal change, a utility used in one place |
| LOW | a leaf React component, a test, docs |

## Pass 2 — Breaking-change detection

A contract is anything a consumer you don't control relies on: a REST response shape, a DB
column, an env var, a data table schema. Adding is usually safe; removing, renaming, tightening,
or retyping usually isn't.

| Safe (non-breaking) | Breaking (needs version bump / migration / coordination) |
|---|---|
| Add an *optional* request field | Make an existing field required, or add a required one |
| Add a field to a response | Remove or rename a response field |
| Add a new endpoint / route | Remove or rename an endpoint, change its URL |
| Relax a required field to optional | Change a field's type (str → int, scalar → list) |
| Add a new enum value *clients tolerate* | Change/remove an enum value; change an HTTP status code |
| Add a *nullable* DB column | Drop a column, add a `NOT NULL` without default, narrow a type |
| Add a table column | Drop/rename a table column, change its type, move a dataset path |

### DRF / REST contracts

```bash
DIFF=/tmp/pr.diff
# Removed or renamed routes
grep "^-" "$DIFF" | grep -E "router\.register|path\(|re_path\("
# Serializer field churn — removed fields break response consumers
grep "^-" "$DIFF" | grep -E "= serializers\.|fields = |read_only|required="
# Status-code changes (clients branch on these)
grep -E "^[-+].*(HTTP_[0-9]{3}|status=[0-9]{3}|Response\(.*status)" "$DIFF"
```

Use the consistent error envelope and status codes the API already follows — `400`
bad request, `401` unauthenticated, `403` authenticated-but-forbidden, `404` missing,
`409` conflict, `422` semantic validation error, `429` rate-limited. Paginate every list
endpoint (DRF `PageNumberPagination`/`LimitOffsetPagination`); an unpaginated list that
grows unbounded is a latent breaking change.

### Django migrations

```bash
git diff "$BASE"...HEAD --name-only | grep -E "migrations/[0-9]"
grep -nE "RemoveField|AlterField|DeleteModel|RunSQL|AddField.*null=False" "$DIFF"
```

- Destructive ops (`RemoveField`, `DeleteModel`, type narrowing) need a two-phase plan:
  ship the additive change + dual-write/read first, drop the old shape in a later release.
- A new `NOT NULL` column needs a `default` or a data migration — otherwise it fails against
  existing rows.
- Confirm the migration is reversible (`reverse_code` / sensible `AlterField` inverse) or
  the rollback story is documented.

### Config / env vars

```bash
git diff "$BASE"...HEAD | grep -E "^\+.*os\.environ|^\+.*env\(|^\+.*getenv" # new vars (must exist in the deploy config)
git diff "$BASE"...HEAD | grep -E "^-.*os\.environ|^-.*getenv"             # removed vars (may be set in prod)
```

A new env var that isn't wired into the deployment config breaks the deploy, not the diff. A removed one may still be set on running
instances. Flag both.

### Data table schema

A changed table/view schema is a contract for every downstream job, dashboard, and
serving endpoint. Treat a dropped/renamed/retyped column exactly like a breaking API change:
identify readers (Pass 1), require the additive-then-remove two-phase rollout.

## Output

Fold findings into the SKILL's severity model — most-exposed surface sets the verdict.
Lead with blast radius and breaking changes; let the linter own style.

```
Blast radius : HIGH — serializers/order.py renders 4 views, 1 React consumer
Breaking     : 1 — `total_cents` renamed to `total` in OrderSerializer (clients break)
Migrations   : 0006 adds NOT NULL `currency` with no default — fails on existing rows

MUST FIX
  1. OrderSerializer field rename is a breaking response change — add the old field
     back as read-only/deprecated, or version the endpoint.
  2. Migration 0006: add default= or a data migration; current form errors on deploy.
SHOULD FIX
  3. New GET /orders list response is unpaginated — add PageNumberPagination.
```

## Pitfalls

- **Reviewing style over substance** — the linter owns style; spend judgment on logic,
  blast radius, and contracts.
- **Missing the indirect break** — the diff looks local but a shared serializer/helper/shared
  table fans out. Always run Pass 1.
- **Migration risk** — `NOT NULL` additions and column drops are the classic prod-breakers.
- **Env var drift** — a new var absent from the deploy config breaks the deploy silently.
- **Splitting late** — if a PR is too large to review properly, ask for it to be split
  before reviewing, not after.
