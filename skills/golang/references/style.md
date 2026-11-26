# Go Style and Design

Judgment calls gofmt cannot make: names, doc comments, interfaces, structs, wiring, package layout, refactoring,
version-gated idioms, golangci-lint v2. Apply to code you write or are asked to change, never to sweep untouched code
or rename established names.

## Naming

| Element | Rule | Yes / no |
| --- | --- | --- |
| Initialism | One case for the whole run | `userID`, `HTTPServer` / `userId`, `Url` |
| Package | Short, lower, one word, singular, a purpose; directory matches (`rate-limit/` → `ratelimit`) | `tabwriter` / `util`, `common` |
| File | Lowercase, `_` between words; `_test`, `_linux` suffixes are semantic | `vendor_file.go` |
| Receiver | 1–2 letters of the type, same on every method | `s *Server` / `this`, `self` |
| Constructor | `New` for a package's one primary type, else `NewT` | `ring.New` / `ring.NewRing` |
| Getter, setter | Field name without `Get`; setter `SetX` | `u.Name()` / `u.GetName()` |
| Bool predicate | `Is`/`Has`/`Can` so it reads as a question | `IsLoopback()` / `Loopback() bool` |
| Interface | One method: method + `-er`; more: a noun or embedding | `Reader`, `ReadCloser` / `IReader` |
| Canonical method | Stdlib names only with stdlib signatures | `String() string` / `ToString()` |
| Enum | Type prefix; zero is `Unknown`, or start at `iota + 1` | `StatusUnknown Status = iota` |
| Error | `ErrX` var, `XError` type; string lowercase incl. initialisms, no final period, may lead `pkg:` | `"config: bad key id"` |
| Variant | `XContext` takes ctx, `MustX` panics, `WithX` is an option | `QueryContext` / `QueryCtx` |
| Import alias | Only on collision or for generated code | `mrand "math/rand/v2"` / `f "fmt"` |

- **No stutter.** The package name is at the call site: `config.Load`, not `config.LoadConfig`; no `util`, `common`,
  `helpers`, `misc`, `base`, `types` packages. Put a helper in the package that uses it, or a narrowly named one
  (`textwrap`) when two areas share it.
- **Renames are scoped work.** A rule-breaking name in code you aren't changing stays; a requested rename goes
  through `gopls rename`, never `sed` (see Refactoring safely).
- **Name compound conditions.** Three or more operands become named booleans (`isOwner := r.OwnerID == u.ID`); keep
  an expensive call inline as the last operand so short-circuiting skips it.
- **Values vs pointers:** pointer to mutate, for large structs (~128 B+), or when nil means "absent".
- **Always:** `net.JoinHostPort` over `fmt.Sprintf("%s:%d")` (IPv6), `errors.Is`/`As` over `==` on wrapped errors.

## Strings

**Never concatenate inside `WriteString` on a `strings.Builder`, `bytes.Buffer` or `bufio.Writer`.** `a + b` builds
a throwaway string first; gopls reports it as `writestring` (golangci-lint does not). One call per piece:

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
- **Doc links** (Go 1.19+): `[Name]`, `[Name.Method]` same package; `[pkg.Name]`, `[*bytes.Buffer]` imported package
  (last path element); `[net/http.Client]` when ambiguous; `[text]` + `// [text]: https://…` for a URL.
- **`Deprecated:`** must open its own paragraph for tools (staticcheck SA1019, gopls); name the replacement:
  `// Deprecated: use [Open].`
- **Examples** are `ExampleX`, `ExampleT_Method`, `ExampleX_suffix` functions in `_test.go` with an `// Output:`
  line so `go test` checks them, not indented code blocks.
- Preview with `go doc -all ./internal/<pkg>` (`go tool doc` is gone since Go 1.26).

## Interfaces and embedding

- **The consumer owns the interface**, naming only the methods it calls; the producer exports a concrete type and
  never imports the interface.
- **Discover, don't design.** One interface, one implementation, no fake = indirection: delete it. An interface earns
  its place from a second production implementation, not a test; for one real boundary (clock, network, exec) a
  package-level func var swapped in the test is often enough. Many mocks signal a missing seam.
- **Compile-time check** only where satisfaction is the point: `var _ io.Writer = (*Sink)(nil)` next to the type.
- **Optional capability via assertion**: keep the parameter minimal, upgrade with comma-ok to a behavior
  (`w.(interface{ Flush() error })`), never a concrete type like `*os.File`.
- Type switches: concrete cases before interface cases (a later concrete case is unreachable); `case nil` when nil is
  valid input. Errors use `errors.As`, never `err.(*T)`.
- A generic body with a type switch inside is an interface in disguise: per-type behavior is an interface; identical
  logic across types is a generic.
- **Embed** only when the outer type *is* the inner one plus more and every promoted method is wanted; otherwise a
  named field exposing only what you delegate. Embedding costs:
  - **API leak:** every exported method becomes yours (`sync.Mutex` publishes `Lock`/`Unlock`); lock in a named,
    unexported field.
  - **Receiver is the inner value:** a promoted method can't see the outer struct; "overriding" only shadows, no
    virtual dispatch.
  - **Silent interface satisfaction:** an embedded interface satisfies the outer type even if never set; calling it
    on nil panics at runtime.
  - **Ambiguous selectors:** two embedded types with the same method name compile until something calls it.
  - **Wire-format churn:** embedded structs flatten into JSON; an upstream field addition shows in your output.

## Structs, construction, wiring

- Never copy a struct holding a mutex or channel (`go vet` copylocks). Don't mutate a config or argument the
  caller still owns.
- Tag every exported field of a serialized type; an untagged rename changes the wire format.
- Make illegal states unrepresentable: an unexported field behind a validating constructor (`NewEmail`) beats a raw
  `string` checked everywhere.
- A nil map field panics on write: allocate in the writing method or the constructor. `NewX` only to enforce an
  invariant, take required dependencies, or return an error.
- **No `init()` for setup, no mutable package globals**: `init()` can't return an error, runs in file-name order
  before tests, and hides dependencies. Compile-once values (`regexp.MustCompile`, `//go:embed`) are fine.

| Shape | Use when |
| --- | --- |
| Positional parameters | Two or three required inputs, none optional: the default |
| Config struct (`Config{...}`) | Several settings, mostly internal callers; zero field = default |
| Functional options (`...Option`) | Published API, many optional knobs, must grow without breaking callers |

- Inside `internal/`, functional options are usually speculative. Invalid options: `Option` returns an error and the
  constructor fails, not first use. Defaults live in code (constructor), never struct-tag magic or reflection.
- **Hand-wire at the composition root** (`main`/`run`): config → infrastructure → stores → services → transport. A
  constructor that opens its own DB or reads env vars hides a dependency: lift it out.
- Pass the specific dependency, never a registry or container. A five-deep chain means a service depends on another
  service where it needed the store. `defer` close in reverse right after a successful open.
- Containers (wire codegen; dig/fx reflection; do generics) pay off only at dozens of services with shared lifecycle;
  below that they move failures from compile time into graph resolution.

## Package boundaries and layout

- **Dependencies point inward.** Domain imports no transport or storage; adapters import the domain.
- **Place by importer count:** several importers → top-level under `internal/`; one area → nested under it
  (`internal/billing/tax`). A file split often fixes a bloated package; split the package only along a real boundary.
- `cmd/<binary>/main.go` is `package main`: parse, wire, call `Run`, exit with a code, no logic. `internal/<area>/`
  holds all application code (the compiler forbids outside imports). `testdata/` sits next to the package that reads
  it and is ignored by the build.
- **No `pkg/`**: it enforces nothing and invites accidental public API. A real library exposes packages at the module
  root, private parts under `internal/`. No `src/`, no root `main.go` in a multi-binary repo, no layer directories
  (`models/`, `services/`) before needed; a flat package is right for a small tool.
- **Breaking import cycles, cheapest first:** (1) consumer-side interface in the importing package (it calls a method
  or two); (2) shared type into a leaf package both import; (3) shared code under a nested `internal/` for siblings;
  (4) mediator package both delegate to, rare. Moving the producer's concrete type into the consumer usually
  relocates the cycle.

## Refactoring safely

- **Structure and behavior never share a commit**; move and optimize are two commits too.
- Untested code gets a characterization test pinning current output before the first edit.
- **Tools over hand edits**: `gopls rename` refuses shadowing and broken interface satisfaction; `gofmt -r`, `eg` or
  a `go/analysis` fixer for repeated rewrites. Call hierarchy first: [tools.md](tools.md).
- **Then grep for what the compiler can't see**: struct tags, `text/template` fields, `reflect` lookups, JSON string
  names, docs and help text.
- Verify each step: `rtk go build ./... && rtk go vet ./...`, touched packages' tests, `-race` for concurrency,
  `benchstat` for hot paths (anything but `~` is a behavior change). Red step → back to the last green commit.
- **Internal code** (this module only): rename, fix every caller, delete the old name in one pass, no alias.
  **Published API:** add new, mark old `// Deprecated: use X`, alias `type Old = pkg.New`; breaking it needs `/vN`.
  Aliases are for gradual repair across modules; inside one module they're a needless shim.

## Modernize — old → new

Apply only rows at or below the `go` line in `go.mod`, only in code the task touches; `go fix ./...` and the
`modernize` analyzer do the mechanical sweep when a sweep is the task. `go test` runs the `stdversion` vet check from
1.27 (flags APIs newer than the `go` directive): bump the directive or drop the call.

| Go | Old | New |
| --- | --- | --- |
| 1.18 | `[]interface{}` + type assertions | type parameters, or the `slices`/`maps` helper that already exists |
| 1.19 | `append(b, fmt.Sprintf(…)...)` | `fmt.Appendf(b, …)` |
| 1.20 | `HasPrefix` then `TrimPrefix` | `after, ok := strings.CutPrefix(s, p)` (also `CutSuffix`; `Cut` since 1.18) |
| 1.20 | multierror / multierr | `errors.Join` |
| 1.21 | hand-rolled `minInt`, delete-all loop | `min`, `max`, `clear` builtins |
| 1.21 | `sort.Slice`, search, clone and map-copy loops | `slices.SortFunc` + `cmp.Compare`, `slices.Contains`, `Clone`; `maps.Clone`, `maps.Copy` |
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
| 1.24 | `context.Background()` in tests; `b.N` loop | `t.Context()`; `for b.Loop()` |
| 1.24 | `runtime.SetFinalizer` | `runtime.AddCleanup` |
| 1.24 | `x/crypto/{sha3,hkdf,pbkdf2}` | `crypto/sha3`, `crypto/hkdf`, `crypto/pbkdf2` |
| 1.24 | `tools.go` blank imports | `tool` directives: `go get -tool …`, `go tool <name>` |
| 1.25 | `wg.Add(1); go func(){ defer wg.Done() … }()` | `wg.Go(func(){ … })` |
| 1.25 | sleeps in concurrency tests, `synctest.Run` | `synctest.Test` + `synctest.Wait` |
| 1.25 | `automaxprocs` import | delete it: `GOMAXPROCS` reads cgroup limits |
| 1.25 | `v.Interface().(T)` | `reflect.TypeAssert[T](v)` |
| 1.26 | `var e *T; errors.As(err, &e)` | `e, ok := errors.AsType[*T](err)` |
| 1.26 | `ptr(v)` helper; `ReverseProxy{Director: …}` | `new(v)` takes an expression; `ReverseProxy{Rewrite: …}` |
| 1.27 | `LastIndex` + slice arithmetic | `strings.CutLast`, `bytes.CutLast` |
| 1.27 | package-level generic helper for one type | a generic method (cannot satisfy an interface) |
| 1.27 | `encoding/json` v1 calls | `encoding/json/v2`; rejects duplicate keys and bad UTF-8: test real payloads |
| 1.27 | `google/uuid` for v4/v7 | stdlib `uuid` (`NewV7()`) once `go mod why` rules out v3/v5, `sql.Scanner` |

The analyzer is `golang.org/x/tools/go/analysis/passes/modernize` (golangci-lint since v2.6.0, gopls, `go fix` since
1.26; `go tool fix help` lists what the toolchain applies). Its fixes preserve behavior, but a scoped run edits every
matching line in the package: review the diff.

```bash
go fix ./internal/<pkg>                         # toolchain-blessed rewrites, scoped
golangci-lint run --enable-only modernize ./... # report only; add --fix on files you own
```

## golangci-lint v2

- **Config.** `version: "2"`; `linters.default: none` plus an explicit `enable` list so a lint bump never adds checks
  silently; knobs under `linters.settings`; formatters in their own `formatters.enable` block, run via
  `golangci-lint fmt`. Exclusions live in `linters.exclusions` (`rules` with `path`/`linters`/`text`/`source`,
  `paths`, `generated: lax`). `run.timeout` defaults to none.
- **`//nolint`:** only a proven false positive or deliberate pattern; name the linter and reason on the same line
  (`nolintlint` `require-specific`, `require-explanation` enforce it); bare `//nolint` is always a finding:

```go
_ = f.Close() //nolint:errcheck // read-only file, close error cannot change the result
```

- Narrowest scope wins: end-of-line over the line above, line over function, never file-wide. A repo-wide recurring
  pattern becomes an `exclusions.rules` entry with a one-line why.
- Never suppress `govet`, `staticcheck`, `bodyclose`, `gosec`, or `errcheck` on a production path without a reason a
  reviewer would accept.
