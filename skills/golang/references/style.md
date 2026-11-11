# Go Style, Naming and Lint

Judgment calls gofmt cannot make: names, control flow, doc comments, version-gated idioms and golangci-lint v2 hygiene.

Harness rules override everything below: comment caps, function length, nesting and the simplify gate live in
[quality.md](../../../rules/standards/quality.md); no renames of established names and no drive-by reformatting live
in [principles.md](../../../rules/standards/principles.md). The rules here apply to code you write or are asked to
change, never as a license to sweep untouched code.

## Naming

| Element | Rule | Yes / no |
| --- | --- | --- |
| Identifier | MixedCaps; `_` only in generated/cgo code, `Test` subcases | `maxRetries` / `MAX_RETRIES` |
| Initialism | One case for the whole run | `userID`, `HTTPServer`, `xmlDoc` / `userId`, `Url` |
| Package | Short, lower, one word, singular; names a purpose | `tabwriter`, `oauth2` / `util`, `common` |
| Directory | Matches the package; a hyphen drops out of the package | `rate-limit/` → `ratelimit` |
| File | Lowercase, `_` between words; `_test`, `_linux` suffixes are semantic | `vendor_file.go` |
| Receiver | 1–2 letters of the type, same on every method | `s *Server` / `this`, `self`, `srv` |
| Constructor | `New` for a package's one primary type, else `NewT` | `ring.New` / `ring.NewRing` |
| Getter, setter | Field name without `Get`; setter `SetX` | `u.Name()` / `u.GetName()` |
| Bool predicate | `Is`/`Has`/`Can` so it reads as a question | `IsLoopback()` / `Loopback() bool` |
| Interface | One method: method + `-er`; more: a noun or embedding | `Reader`, `ReadCloser` / `IReader` |
| Canonical method | Stdlib names only with stdlib signatures | `String() string` / `ToString()` |
| Constant | MixedCaps, named for the role, not the value | `DefaultPort` / `PORT_8080` |
| Enum | Type prefix; zero is `Unknown`, or start at `iota + 1` | `StatusUnknown Status = iota` |
| Error var, type | `ErrX` var, `XError` type | `ErrNotFound`, `PathError` |
| Error string | Lowercase incl. initialisms, no final punctuation; may lead `pkg:` | `"vault: bad key id"` |
| Format func | `f` suffix when args are a format string | `Errorf`, `Logf` |
| Variant | `XContext` takes ctx, `MustX` panics, `WithX` is an option | `QueryContext` / `QueryCtx` |
| Import alias | Only on collision or for generated code | `mrand "math/rand/v2"` / `f "fmt"` |

- **No stutter.** The package name is always at the call site: `http.Client`, `config.Load`, `dbpool.Status`, never
  `http.HTTPClient`, `config.LoadConfig`, `dbpool.PoolStatus`. Same for context: a `UserStore.Create(name)`, not
  `CreateUser(userName)`.
- **Length tracks scope.** `i`, `v`, `n`, `r`, `w`, `b`, `t`, `ctx`, `err` in a few-line scope; descriptive names at
  package level. Never encode the type (`userSlice`, `nameStr`); one name per domain concept across the codebase.
- **Parameters document the call site.** Short when the type explains it (`d time.Duration`), descriptive when it
  doesn't (`Unix(sec, nsec int64)`, `HasPrefix(s, prefix string)`).
- **Named results** only to disambiguate same-typed returns (`(written int64, err error)`); never to enable a bare
  `return` outside a 2–3 line function.
- **Tests.** `TestParse`, `TestServer_Handle`, `TestParse_EmptyInput`; `t.Run` names are lowercase phrases
  (`"invalid id"`); helpers call `t.Helper()` and a panicking fixture loader is `mustLoadX`. Table fields follow the
  file's existing `want`/`got` or `expected` style.
- **Renames are scoped work.** A name that breaks these rules in code you aren't changing stays; a rename the task
  does ask for goes through `gopls rename` (every call site, interface-safe), never `sed`.

## Control flow and shape

- **Guard clauses first, happy path flush left.** Handle error and edge cases with an early `return`/`continue`; once an
  `if` body ends in `return`, `break` or `continue`, drop the `else`. This is how you hold nesting at 4 or less.
- **Default then override** instead of an `if/else if/else` assignment chain:

```go
level := slog.LevelInfo
switch {
case debug:
    level = slog.LevelDebug
case quiet:
    level = slog.LevelWarn
}
```

- **Switch** when the same value is compared more than twice; tagless `switch` for unrelated predicates.
- **Name compound conditions.** Three or more operands become named booleans (`isOwner := r.OwnerID == u.ID`), but
  keep an expensive call inline as the last operand so short-circuiting still skips it.
- **Scope to the `if`.** `if err := v.Check(); err != nil { … }` when the value is only needed for the test.
- **Signatures.** `ctx context.Context` first, inputs, then output sinks. Past ~4 params, take an options struct; a
  long signature is a design smell, not a wrapping problem.
- **Declarations.** `var x T` signals "starts at zero" (`var buf bytes.Buffer`); `:=` for anything initialized.
  Composite literals use field names. Prefer a nil slice (`var out []T`) unless the zero value leaks out as JSON
  `null` where callers expect `[]`; preallocate (`make([]T, 0, len(in))`) only when the length is known.
- **Values vs pointers.** Pass strings, ints, `time.Time` and small read-only structs by value; use a pointer to
  mutate, for large structs (~128 B+) or when nil means "absent". Benchmark before arguing either way.
- **Strings.** `strconv` for single conversions, `fmt` for real formatting, `strings.Builder` in loops, `%q` for
  user-supplied values in errors.
- **Surface.** Unexport by default (exporting later is free, unexporting breaks callers). No dot imports; blank
  imports only in `main` and tests. Generics over `any` when one type parameter covers the cases; no `reflect`
  without a reason the code states.

## Doc comments

The harness cap is one line per comment ([quality.md → Comments](../../../rules/standards/quality.md#comments)); godoc
conventions fit inside it.

- **Exported symbols get one sentence** that begins with the identifier and says what it does or guarantees:
  `// Load reads the vault at path and fails closed on a bad key.` Unexported code gets a comment only when the name
  can't carry the intent. Test functions get none.
- **Say what the signature can't:** the error returned and when, a panic, concurrency safety, a non-obvious
  precondition. Never paraphrase the name (`// GetUser gets a user`), restate types or add adjectives.
- **Package comment:** one `// Package x …` line above `package x` in one file; a longer package narrative goes to the
  repo's own docs, not `doc.go` prose.
- **Doc links** (Go 1.19+) render as hyperlinks on pkg.go.dev and in `go doc`; use them instead of naming a symbol in
  backticks:

| Syntax | Links to |
| --- | --- |
| `[Name]`, `[Name.Method]` | A symbol in the same package |
| `[pkg.Name]`, `[*bytes.Buffer]` | A symbol in an imported package (import path's last element) |
| `[net/http.Client]` | Fully qualified, when the short form is ambiguous |
| `[text]` + `// [text]: https://…` | A URL via a link definition at the end of the comment |

- **`Deprecated:`** must open its own paragraph for tools (staticcheck SA1019, gopls) to see it; it is a semantic
  marker the harness exempts from the one-line cap. Name the replacement: `// Deprecated: use [Open].`
- **Examples** live in `ExampleX`, `ExampleT_Method`, `ExampleX_suffix` functions in `_test.go` with an `// Output:`
  line so `go test` checks them, never as indented code blocks inside the comment.
- **Directives** (`//go:build`, `//go:embed`, `//nolint`) have no space after `//` and are not doc text.
- Preview with `go doc -all ./internal/<pkg>` (`go tool doc` is gone since Go 1.26).

## Modernize — old → new

This module targets **Go 1.27** ([go.mod](../../../go.mod)), so every row applies. Rewrite only in code the task
touches; `go fix ./...` and the `modernize` analyzer do the mechanical sweep when a sweep is the task.

| Go | Old | New |
| --- | --- | --- |
| 1.18 | `interface{}` | `any` |
| 1.18 | `[]interface{}` + type assertions | type parameters, or the `slices`/`maps` helper that already exists |
| 1.19 | `append(b, fmt.Sprintf(…)...)` | `fmt.Appendf(b, …)` |
| 1.20 | `HasPrefix` then `TrimPrefix` | `after, ok := strings.CutPrefix(s, p)` (also `CutSuffix`; `Cut` since 1.18) |
| 1.20 | multierror / multierr | `errors.Join` |
| 1.21 | hand-rolled `minInt`, delete-all loop | `min`, `max`, `clear` builtins |
| 1.21 | `sort.Slice`, search and clone loops | `slices.SortFunc` + `cmp.Compare`, `slices.Contains`, `Clone` |
| 1.21 | map copy loop | `maps.Clone`, `maps.Copy` |
| 1.21 | `sync.Once` + package var | `sync.OnceValue`, `sync.OnceValues`, `sync.OnceFunc` |
| 1.21 | zap/logrus in new code | `log/slog` |
| 1.21 | detaching ctx by hand | `context.WithoutCancel`, `context.AfterFunc`, `WithTimeoutCause` |
| 1.22 | `for i := 0; i < n; i++` | `for i := range n` |
| 1.22 | `v := v` before a closure | delete it: each iteration has its own variable |
| 1.22 | `math/rand`, `rand.Seed` | `math/rand/v2` (`IntN`, `rand.N`); no seeding |
| 1.22 | `if x == "" { x = d }` | `x = cmp.Or(x, d)` |
| 1.22 | gorilla/chi for method+path | `mux.HandleFunc("GET /users/{id}", h)` + `r.PathValue("id")` |
| 1.22 | `reflect.TypeOf((*I)(nil)).Elem()` | `reflect.TypeFor[I]()` |
| 1.23 | collect-then-return slices | `iter.Seq`/`Seq2`; `slices.Sorted(maps.Keys(m))`, `Collect`, `Chunk` |
| 1.23 | `defer t.Stop()` only for GC | drop it: unreferenced timers are collected |
| 1.24 | `filepath.Join(base, userPath)` | `os.OpenRoot(base)` then `root.Open(userPath)`; no traversal |
| 1.24 | `omitempty` on `time.Time`/structs | `omitzero` |
| 1.24 | `for _, p := range strings.Split(s, ",")` | `strings.SplitSeq`, `FieldsSeq`, `Lines` |
| 1.24 | `context.Background()` in tests | `t.Context()` |
| 1.24 | `for i := 0; i < b.N; i++` | `for b.Loop()` |
| 1.24 | `runtime.SetFinalizer` | `runtime.AddCleanup` |
| 1.24 | `x/crypto/{sha3,hkdf,pbkdf2}` | `crypto/sha3`, `crypto/hkdf`, `crypto/pbkdf2` |
| 1.24 | `tools.go` blank imports | `tool` directives: `go get -tool …`, `go tool <name>` |
| 1.25 | `wg.Add(1); go func(){ defer wg.Done() … }()` | `wg.Go(func(){ … })` |
| 1.25 | sleeps in concurrency tests, `synctest.Run` | `synctest.Test` + `synctest.Wait` |
| 1.25 | `automaxprocs` import | delete it: `GOMAXPROCS` reads cgroup limits |
| 1.25 | `v.Interface().(T)` | `reflect.TypeAssert[T](v)` |
| 1.26 | `var e *T; errors.As(err, &e)` | `e, ok := errors.AsType[*T](err)` |
| 1.26 | `ptr(v)` helper | `new(v)` takes an expression |
| 1.26 | `ReverseProxy{Director: …}` | `ReverseProxy{Rewrite: …}` |
| 1.27 | `LastIndex` + slice arithmetic | `strings.CutLast`, `bytes.CutLast` |
| 1.27 | package-level generic helper for one type | a generic method (cannot satisfy an interface) |
| 1.27 | `encoding/json` v1 calls | `encoding/json/v2`; rejects duplicate keys and bad UTF-8: test real payloads |
| 1.27 | `google/uuid` for v4/v7 | stdlib `uuid` (`NewV7()`) once `go mod why` rules out v3/v5, `sql.Scanner` |

- **Always:** `net.JoinHostPort` over `fmt.Sprintf("%s:%d")` (IPv6), `errors.Is`/`As` over `==` on wrapped errors.
- **Version bumps:** `go test` runs the `stdversion` vet check from 1.27, flagging APIs newer than the `go`
  directive; bump the directive or drop the call, never silence it. A `godebug` pin of a removed setting fails the
  build.

## The `modernize` analyzer

- Source: `golang.org/x/tools/go/analysis/passes/modernize`; in golangci-lint since v2.6.0, also in gopls and behind
  `go fix` (rebuilt on `go/analysis` in 1.26). Coverage differs per tool version: `go tool fix help` lists what the
  installed toolchain applies.
- It is already enabled in [.golangci.yml](../../../.golangci.yml). Its fixes are behavior-preserving by construction,
  so `golangci-lint run --fix --enable-only modernize ./internal/<pkg>` is safe on files you own; review the diff
  anyway, since a scoped run still edits every matching line in the package.

```bash
go fix ./internal/<pkg>                         # toolchain-blessed rewrites, scoped
golangci-lint run --enable-only modernize ./... # report only
```

## golangci-lint v2

- **Config essentials.** `version: "2"` at the top; `linters.default: none` plus an explicit `enable` list (this repo's
  stance) so a lint bump never silently adds checks; per-linter knobs under `linters.settings`; formatters are their
  own `formatters.enable` block and run via `golangci-lint fmt`. Exclusions moved to `linters.exclusions`
  (`rules` with `path`/`linters`/`text`/`source`, `paths`, `generated: lax`). `run.timeout` defaults to none.
- **Migrating** a v1 file: `golangci-lint migrate`; validate any edit with `golangci-lint config verify`.
- **Run order:** `make lint-go` (or `golangci-lint run ./internal/<pkg>/...` while iterating), then
  `golangci-lint fmt`. Read the trailing `(linter)` in each `file:line:col: msg (linter)` hit before acting.
- **Legacy adoption:** `issues.new-from-rev` gates only changed lines; never bump a threshold to go green.
- **Thresholds** come from [quality.md](../../../rules/standards/quality.md) (functions < 50 lines, complexity > 10
  flagged), not upstream defaults; the repo's `gocognit` setting is its own call.
- **Linters worth knowing** when a hit appears: `govet`/`staticcheck` (real bugs, deprecations), `errcheck`, `errorlint`
  (`%w`, `errors.Is`), `bodyclose`/`noctx` (HTTP leaks, missing ctx), `perfsprint`, `revive` (naming, stutter),
  `errname`, `predeclared`, `intrange`, `copyloopvar`, `usetesting` (`t.TempDir`/`t.Setenv`), `nolintlint`.

### `//nolint` discipline

- Fix first; suppress only a proven false positive or a deliberate pattern, and say which in the reason.
- Always name the linter and give the reason on the same line, which `nolintlint`
  (`require-specific`, `require-explanation`) enforces:

```go
_ = f.Close() //nolint:errcheck // read-only file, close error cannot change the result
```

- Narrowest scope wins: end-of-line over the line above, line over function, never file-wide. A pattern that recurs
  across the repo becomes an `exclusions.rules` entry in [.golangci.yml](../../../.golangci.yml) with a one-line why.
- Never suppress `govet`, `staticcheck`, `bodyclose`, `gosec`, or `errcheck` on a production path without a reason a
  reviewer would accept; bare `//nolint` is always a finding.
- Delete a directive once it suppresses nothing (`nolintlint` reports `unused`).

Adapted from samber/cc-skills-golang (MIT): golang-code-style, golang-naming, golang-documentation,
golang-modernize, golang-lint.
