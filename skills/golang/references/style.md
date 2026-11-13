# Go Style, Naming and Lint

Judgment calls gofmt cannot make: names, control flow, doc comments, version-gated idioms, golangci-lint v2. Apply to
code you write or are asked to change, never to sweep untouched code or rename established names.

## Naming

| Element | Rule | Yes / no |
| --- | --- | --- |
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
| Enum | Type prefix; zero is `Unknown`, or start at `iota + 1` | `StatusUnknown Status = iota` |
| Error var, type | `ErrX` var, `XError` type | `ErrNotFound`, `PathError` |
| Error string | Lowercase incl. initialisms, no final punctuation; may lead `pkg:` | `"config: bad key id"` |
| Variant | `XContext` takes ctx, `MustX` panics, `WithX` is an option | `QueryContext` / `QueryCtx` |
| Import alias | Only on collision or for generated code | `mrand "math/rand/v2"` / `f "fmt"` |

- **No stutter.** The package name is at the call site: `config.Load`, not `config.LoadConfig`; no `util`, `common`,
  `helpers`, `misc`, `base`, `types` packages.
- **Renames are scoped work.** A rule-breaking name in code you aren't changing stays; a requested rename goes
  through `gopls rename`, never `sed` ([design.md](design.md) → Refactoring safely).

## Control flow and shape

- **Name compound conditions.** Three or more operands become named booleans (`isOwner := r.OwnerID == u.ID`); keep
  an expensive call inline as the last operand so short-circuiting skips it.

## Strings

**Never concatenate inside `WriteString` on a `strings.Builder`, `bytes.Buffer` or `bufio.Writer`.** `a + b` builds
a throwaway string first; gopls reports it as `writestring` (golangci-lint does not). One call per piece.

```go
b.WriteString(prefix + name + "\n") // WRONG: allocates the joined string first

b.WriteString(prefix) // RIGHT
b.WriteString(name)
b.WriteString("\n")
```

- Constants joined at compile time are fine as one call: `b.WriteString(header + "\n")` with a `const header`.
- **A file, pipe or socket keeps the single call** (`f.WriteString(key + "\n")`): each call is its own write, so
  splitting costs a system call and lets another writer land between the pieces.
- With formatting verbs, `fmt.Fprintf(&b, ...)`, never `b.WriteString(fmt.Sprintf(...))`.

## Doc comments

- **Say what the signature can't:** the error returned and when, a panic, concurrency safety, a non-obvious
  precondition. Never paraphrase the name (`// GetUser gets a user`).
- **Doc links** (Go 1.19+) render as hyperlinks on pkg.go.dev and in `go doc`:

| Syntax | Links to |
| --- | --- |
| `[Name]`, `[Name.Method]` | A symbol in the same package |
| `[pkg.Name]`, `[*bytes.Buffer]` | A symbol in an imported package (import path's last element) |
| `[net/http.Client]` | Fully qualified, when the short form is ambiguous |
| `[text]` + `// [text]: https://…` | A URL via a link definition at the end of the comment |

- **`Deprecated:`** must open its own paragraph for tools (staticcheck SA1019, gopls); name the replacement:
  `// Deprecated: use [Open].`
- **Examples** are `ExampleX`, `ExampleT_Method`, `ExampleX_suffix` functions in `_test.go` with an `// Output:`
  line so `go test` checks them, not indented code blocks.
- Preview with `go doc -all ./internal/<pkg>` (`go tool doc` is gone since Go 1.26).

## Modernize — old → new

Apply only rows at or below the `go` line in `go.mod`, only in code the task touches; `go fix ./...` and the
`modernize` analyzer do the mechanical sweep when a sweep is the task.

| Go | Old | New |
| --- | --- | --- |
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

- **Values vs pointers:** pointer to mutate, for large structs (~128 B+), or when nil means "absent".
- **Version bumps:** `go test` runs the `stdversion` vet check from 1.27 (flags APIs newer than the `go` directive):
  bump it or drop the call.
- **Always:** `net.JoinHostPort` over `fmt.Sprintf("%s:%d")` (IPv6), `errors.Is`/`As` over `==` on wrapped errors.

## The `modernize` analyzer

- `golang.org/x/tools/go/analysis/passes/modernize`; in golangci-lint since v2.6.0, in gopls, and behind `go fix`
  (rebuilt on `go/analysis` in 1.26). Coverage varies per version: `go tool fix help` lists what the toolchain applies.
- Its fixes are behavior-preserving, so `golangci-lint run --fix --enable-only modernize ./internal/<pkg>` is safe on
  files you own; review the diff anyway, since a scoped run edits every matching line in the package.

```bash
go fix ./internal/<pkg>                         # toolchain-blessed rewrites, scoped
golangci-lint run --enable-only modernize ./... # report only
```

## golangci-lint v2

- **Config.** `version: "2"`; `linters.default: none` plus an explicit `enable` list so a lint bump never adds checks
  silently; knobs under `linters.settings`; formatters in their own `formatters.enable` block, run via
  `golangci-lint fmt`. Exclusions live in `linters.exclusions` (`rules` with `path`/`linters`/`text`/`source`,
  `paths`, `generated: lax`). `run.timeout` defaults to none.

### `//nolint` discipline

- Suppress only a proven false positive or deliberate pattern; name the linter and the reason on the same line
  (`nolintlint` `require-specific`, `require-explanation` enforce it):

```go
_ = f.Close() //nolint:errcheck // read-only file, close error cannot change the result
```

- Narrowest scope wins: end-of-line over the line above, line over function, never file-wide. A repo-wide recurring
  pattern becomes an `exclusions.rules` entry with a one-line why.
- Never suppress `govet`, `staticcheck`, `bodyclose`, `gosec`, or `errcheck` on a production path without a reason a
  reviewer would accept; bare `//nolint` is always a finding.
