# PR & contract review — blast radius and breaking changes

Two judgment passes the detectors can't make: **what else could this change break** (blast radius) and **does it
break a published contract** (breaking changes). Examples use Django/DRF, a data warehouse and React; substitute
your stack. For GitHub PRs use `gh pr diff <id>`. Run the full treatment on a PR over ~200 changed lines or one
touching a shared module, serializer, migration or shared table; scope first:

```bash
BASE=$(git merge-base HEAD main)
git diff "$BASE"...HEAD --stat        # what moved, how much
git diff "$BASE"...HEAD --name-only   # bare file list for the passes below
```

## Pass 1 — Blast radius

For each changed file ask *who depends on this* before judging it in isolation; a 5-line edit to a shared helper can
break every stage that imports it.

```bash
grep -rn "from mypkg.pipelines import\|import mypkg.pipelines" --include="*.py" .   # Python importers
grep -rn '"example.com/mymod/internal/pipelines"' --include="*.go" .                # Go importers
grep -rn "MySerializer\|MyViewSet" --include="*.py" src/                            # DRF consumers
grep -rn "from ['\"].*useBindingData['\"]" --include="*.ts" --include="*.tsx" src/  # React/TS importers
grep -rn "schema.my_table\|/data/my_dataset" --include="*.py" --include="*.sql" .   # data readers
```

The most exposed file sets the review bar:

| Blast radius | Surface |
|---|---|
| CRITICAL | public API contract, DB model/migration, auth/token plumbing, a table other jobs read, a shared pipeline stage |
| HIGH | a module imported by 3+ others, shared settings/env var, a job other jobs depend on |
| MEDIUM | single-module internal change, a utility used in one place |
| LOW | a leaf React component, a test, docs |

## Pass 2 — Breaking-change detection

A contract is anything a consumer you don't control relies on: a REST response shape, a DB column, an env var, a
table schema. Adding is usually safe; removing, renaming, tightening or retyping usually isn't.

| Safe (non-breaking) | Breaking (needs version bump / migration / coordination) |
|---|---|
| Add an *optional* request field | Make an existing field required, or add a required one |
| Add a field to a response | Remove or rename a response field |
| Add a new endpoint / route | Remove or rename an endpoint, change its URL |
| Relax a required field to optional | Change a field's type (str → int, scalar → list) |
| Add a new enum value *clients tolerate* | Change/remove an enum value; change an HTTP status code |
| Add a *nullable* DB column | Drop a column, add a `NOT NULL` without default, narrow a type |
| Add a table column | Drop/rename a table column, change its type, move a dataset path |

**DRF / REST** — grep the diff's removed lines:

```bash
grep "^-" "$DIFF" | grep -E "router\.register|path\(|re_path\("                       # removed/renamed routes
grep "^-" "$DIFF" | grep -E "= serializers\.|fields = |read_only|required="             # serializer field churn
grep -E "^[-+].*(HTTP_[0-9]{3}|status=[0-9]{3}|Response\(.*status)" "$DIFF"             # status-code changes
```

Keep the API's existing error envelope and status codes (`400` bad request, `401` unauthenticated, `403` forbidden,
`404` missing, `409` conflict, `422` validation, `429` rate-limited). Paginate every list endpoint; an unbounded
list is a latent breaking change.

**Django migrations** — `grep -nE "RemoveField|AlterField|DeleteModel|RunSQL|AddField.*null=False" "$DIFF"`:

- Destructive ops (`RemoveField`, `DeleteModel`, type narrowing) need a two-phase plan: ship the additive change
  with dual-write/read first, drop the old shape in a later release.
- A new `NOT NULL` column needs a `default` or a data migration, or it fails on existing rows.
- Confirm the migration is reversible, or the rollback story is documented.

**Config / env vars** — a new var absent from the deploy config breaks the deploy, not the diff; a removed one may
still be set on running instances. Flag both:
`git diff "$BASE"...HEAD | grep -E "^[-+].*(os\.environ|getenv|env\()"`.

**Data table schema** — a dropped/renamed/retyped column is a breaking API change for every downstream job and
dashboard: identify readers (Pass 1), require the additive-then-remove two-phase rollout.

## Output

Lead with blast radius and breaking changes; the most exposed surface sets the verdict (the linter owns style). If
a PR is too large to review properly, ask for a split before reviewing.

```text
Blast radius : HIGH — serializers/order.py renders 4 views, 1 React consumer
Breaking     : 1 — `total_cents` renamed to `total` in OrderSerializer (clients break)
Migrations   : 0006 adds NOT NULL `currency` with no default — fails on existing rows

MUST FIX
  1. OrderSerializer field rename is a breaking response change — keep the old field read-only/deprecated, or version it.
  2. Migration 0006: add default= or a data migration.
SHOULD FIX
  3. New GET /orders list response is unpaginated — add PageNumberPagination.
```
