# PR & contract review — blast radius and breaking changes

For PRs over ~200 lines or changes to shared modules, serializers, migrations or shared tables, run both passes.
Scope with `rtk git diff "$(git merge-base HEAD main)"...HEAD --stat`; GitHub: `claude-code worktree pr diff <id>`.
Examples use Django/DRF, warehouse tables and React; adapt to the stack.

## Pass 1 — Blast radius

For each changed file, use `git grep -n` to find importers, serializer/view consumers, hooks, table/dataset readers.
The most exposed surface sets the review bar:

- **CRITICAL:** public API, DB model/migration, auth/token plumbing, externally read table, shared pipeline stage.
- **HIGH:** module with 3+ importers, shared settings/env, job other jobs depend on.
- **MEDIUM:** single-module internal change or single-use utility.
- **LOW:** leaf React component, tests, docs.

## Pass 2 — Breaking-change detection

A contract is anything an uncontrolled consumer relies on: response shape, column, env variable, table schema.
Additions are usually safe; removals, renames, tightening and type changes need versioning, migration or coordination.

| Usually safe | Breaking |
| --- | --- |
| Optional request field | New required field or optional → required |
| Response field/endpoint addition | Field/endpoint removal, rename or URL change |
| Required → optional | Type change (str → int, scalar → list) |
| Enum addition clients tolerate | Changed/removed enum or HTTP status |
| Nullable DB column | Dropped column, NOT NULL without default, narrowed type |
| Table column addition | Dropped/renamed/retyped column or moved dataset |

**REST:** inspect removed routes and serializer fields, `read_only`/`required` changes and changed status codes.
Keep the existing error envelope/statuses: 400 bad request, 401 unauthenticated, 403 forbidden, 404 missing,
409 conflict, 422 validation, 429 rate-limited. Paginate every list endpoint; unbounded lists are latent breakage.

**Migrations:** inspect `RemoveField`, `AlterField`, `DeleteModel`, `RunSQL`, `AddField` with `null=False`.
Destructive ops need two phases: additive change with dual read/write, then drop the old shape in a later release.
New NOT NULL columns need a default or data migration. Confirm reversibility or document rollback.

**Config/env:** flag new variables missing from deployment config and removed variables still set on running instances.
**Data schema:** find all downstream jobs/dashboards before dropping, renaming or retyping; require two-phase rollout.

## Output

Lead with blast radius, breaking changes and migrations, then must-fix/should-fix items with concrete fixes.
The most exposed surface sets the verdict; the linter owns style. If too large to review, ask for a split first.
