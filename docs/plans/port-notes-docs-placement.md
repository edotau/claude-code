# Port notes — docs-placement gate

Ported from claude-dbx (`internal/hooks/docsplacement.go` + rule text at
`rules/harness/docs-placement.md`) into this repo's `internal/hooks/docsplacement.go`.

## Section to paste into `rules/standards/documentation.md`

```markdown
## docs-placement

A `PreToolUse(Write|Edit)` hook (`internal/hooks/docsplacement.go`) blocks a new `.md`/`.markdown`
write at a repo ROOT, or under a **gitignored** `<repo>/docs` (invisible on a fresh clone). Root
anchors (README/CLAUDE/AGENTS/GEMINI/LICENSE/CONTRIBUTING/CHANGELOG/…), git-tracked files, and
anything under this harness's own `docs/` tree pass unconditionally. A blocked write names the
canonical home instead: `docs/plans/`, `docs/handoffs/`, `docs/reports/`, `docs/memory/` under
`~/.claude`. Disable with `HARNESS_DOCS_PLACEMENT=0`; a genuine repo doc the user asked for passes
once `git add -N <file>` makes it tracked.
```

## Registration (parent wires these — not touched by this port)

`internal/hooks/io.go` `Registry` map:

```go
"docs-placement": func(in io.Reader, _, errw io.Writer) int { return DocsPlacement(in, errw) },
```

`internal/hookspec/hookspec.go` `Registry` slice:

```go
{Name: "docs-placement", Event: EventPreToolUse, Matcher: "Write|Edit", Timeout: 5},
```

## Adaptation notes

- Knob chain (knobs.go / knobvalue.go / env.d/hooks.env / settings.json env) replaced with a single
  `os.Getenv("HARNESS_DOCS_PLACEMENT") == "0"` check — this repo has no knob system.
- `claude-dbx/internal/git` has no equivalent package here; `isTracked`/`isIgnored` call `exec.Command`
  directly (matching the existing style in `stop_format.go`), and `repoRootOf` reuses
  `internal/memory.GitToplevel` (already used by `plan.go`) instead of a dedicated `git.Toplevel`.
- `claude-dbx/internal/fsutil.IsDir` has no equivalent here; added a package-local `isDir`.
- Dropped `repoRootOrCWD` — in dbx it exists only for `plan.go`'s artifact tagging, not for the gate
  itself; this repo's `plan.go` already tags via `cmp.Or(memory.GitToplevel(cwd), cwd)` directly, so
  porting an unused helper into `docsplacement.go` would be dead weight.
- Dropped the Copilot-vendor JSON-deny test (`TestDocsPlacementHandlerDenyJSONPerVendor`) and its
  `placementPayloadCopilot` helper — this repo's `hooks.Input`/`Handler` carry no vendor dispatch
  (`Vendor()`, `DenyJSON`), so there is nothing to test.
- Block message points at `rules/standards/documentation.md` (no `rules/harness/` tier here) and
  drops the "every linked workspace reaches `.claude/docs` via a symlink" framing — this repo has no
  per-workspace `workspace link` symlink concept; `~/.claude/docs` is referenced by its one real path.
- `paths.DocsPlansDir()` / `DocsReportsDir()` / `DocsHandoffsDir()` / `HarnessDocsDir()` have no
  equivalents in this repo's `internal/paths`; the port inlines `filepath.Join(paths.ConfigDir(), "docs", …)`,
  matching the existing pattern in `plan.go`.

## Bugs found in dbx source

None. Reviewed `canonDeep`, `repoRootOf`, `isTracked`/`isIgnored` pathspec handling, the
`atRoot`/`inDocs` gated-shape logic, and the symlink-realpath assumption behind
`git rev-parse --show-toplevel` (git resolves this to a realpath, consistent with the existing
`TestDocsPlacementThroughSymlink` coverage) — no incorrect field read, dead branch, or off-by-one.
