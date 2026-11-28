---
paths:
  - "internal/**/*.go"
  - "cmd/claude-code/**"
---
# Go in the claude-code harness

These conventions for `github.com/edotau/claude-code` override the general golang skill; `standards/` still applies.

## Layout and placement

- `cmd/claude-code/main.go` wires and exits; logic lives in `internal/<area>/`.
- `internal/cli` owns the command table, menu, dispatch, flags and output. A verb is `cmd<Verb>` in its area's
  CLI file or a table entry into its package; small verbs share a file.
- `internal/` groups packages into 11 domains: `agent`, `assets`, `cli`, `dev`, `driver`, `hooks`, `memory`,
  `platform`, `proxy`, `remote`, `routing`. Box each external SDK in one package under `driver/` or its domain.
- Shared helpers live under their domain (`internal/platform/httpx`, `internal/remote/egress`), not flat at
  `internal/` root; one area's private helper nests under it.
- **Layout is memory:** one concept has one name across command/package, settings/type/table/vault/configure,
  and package/techdebt/memory area. Fix a name/place mismatch instead of recording it in memory; mirroring is a guide.
- Scratch writers use `state/harness/<area>/` or the git dir, never a repo root; ignoring the file is not a fix.
- `state/vault.db`: untracked secrets; `state/certs/`: CA bundles; `state/harness/<owner>/`: schema-versioned state
  migrated on read; `state/harness/backups/`: plaintext backups via `paths.Backup`. Doctor reports formats.
- Outside state: machine-wide `/tmp/claude-code-bench-<uid>` lock, `$TMPDIR/claude-code-verify` trees,
  `~/.claude/run/clip.sock`, bin, plans, memory, settings and vendor homes.
- `docs/templates/` indexes embedded templates. `container/` holds the Ubuntu podman image
  (`make image|image-audit|image-shell`); rootless Linux installs Tailscale static binaries without starting its daemon.
- TypeScript mods: slash-usage, dispatch-spawn, codemode (`/codemode on|off|status`); build with `tsc -p mods/<name>`.
  agents-band is unloaded by default; add its path to `CLAUDE_CODE_PLUGIN_DIRS`, then `/reload-plugins`.
  It shows leaf claims/notes and free dirty files (`agents tree --files`).
- No DI container, cobra or viper: wire explicitly in `Run`; parse config in the area's package.

## CLI and TUI text

Terminal help and TUI rows stay on one line, ≤80 characters including the label; clip wider rows
(`...` in help, `…` in menus). Piped help keeps full text, wrapping only at a hanging description indent.

- Lead with the action and the fact needed to choose; cut filler and chained clauses.
- One width test per menu table, using the real renderer.
- Help uses `helptext.Page`, `helptext.NewFlagSet` and `helptext.Parse`; FLAGS rows come from the FlagSet.

## Tests

- `foo.go` has one `foo_test.go`, including benchmarks, fuzz and examples, ordered by subject.
  Use `foo_regression_test.go` only for 2+ regressions. Every test file maps to an existing source, uses internal
  `package foo` and has no build tags; only `internal/memory/jobscope_test.go` is exempt (import cycle).
  `TestMain` goes in the main source's test file. Merge a source with 0–1 tests and no distinct role into its sibling.
- Fix test import cycles in production code with a `SwapX`/`StubX` seam or a shared package.
  Register importer fixture rows in `TestMain`; missing rows fail.
- Put `go test -c`, CPU/memory profiles and the test binary under scratch paths, using `-o`; never in the repo.
- Use portable calls and runtime OS skips so Windows vet compiles. Shrink production timeouts through package vars
  with proportional bounds. Seams, mocks and speed: `rules/standards/testing-guidelines.md` → A test must guard.

## Style and lint

- Godoc follows `rules/standards/quality.md` → Comments; `Deprecated:` paragraphs are exempt.
  Package narrative belongs in README/docs, not `doc.go`.
- `.golangci.yml`: `linters.default: none`, explicit enables including `modernize`, `gocognit` threshold 30.
  `claude-code review simplify --fail` enforces quality caps and test-file layout.
- Process-global env/cwd changes go through `internal/platform/proc`; direct calls are allowed only there, in
  `internal/platform/hosttest` and tests. Per-run values use `proc.WithValues`/`agent.RunValues`.
- CLI lint does not include gopls checks such as `writestring`; also check editor diagnostics.
- Verify dependency APIs with `go doc <pkg>.<Sym>` or the module cache, never memory.
- Follow `quality.md` → Verification Order and `standing-requirements.md` R16/R32 for test scope.
  Check portability with `GOOS=linux rtk go build ./...` and `GOOS=windows go vet ./...`.
