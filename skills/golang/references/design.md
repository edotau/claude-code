# Go Design and Layout

Scope: types, interfaces, constructors, dependency wiring, package boundaries and safe restructuring in Go code.

Harness rules win on conflict: [CLAUDE.md](../../../CLAUDE.md) → Where things live (layout),
[principles.md](../../../rules/standards/principles.md) (simplicity first, surgical changes) and
[quality.md](../../../rules/standards/quality.md) (clean breaks in internal code, no shims).

## Interfaces

- **Accept interfaces, return structs.** A narrow parameter type lets callers pass anything (a fake included); a
  concrete return keeps every method and field visible, and the caller can still narrow it on its side.
- **The consumer owns the interface.** Declare it in the package that calls it, naming only the methods it calls.
  The producer exports a concrete type and never imports or knows about the interface.
- **Discover, don't design.** No interface until a second implementation or a real boundary (network, clock,
  external CLI) demands one. One interface, one implementation, no fake = pure indirection: delete it.
- **Small: one to three methods.** Need more? Compose small ones (`io.ReadWriteCloser`) rather than declaring a
  wide contract every implementation must fully satisfy.
- **Honor canonical signatures**: `String() string`, `Error() string`, `MarshalJSON() ([]byte, error)`. A
  `ToString()` misses `fmt.Stringer` silently.
- **Compile-time check** only where satisfaction is the point and nothing else would catch a break:
  `var _ io.Writer = (*Sink)(nil)` next to the type.
- **Optional capability via assertion**: keep the parameter minimal and upgrade at runtime; always comma-ok.

```go
func write(w io.Writer, p []byte) error {
    if _, err := w.Write(p); err != nil {
        return err
    }
    if f, ok := w.(interface{ Flush() error }); ok { // assert to behavior, never to *os.File
        return f.Flush()
    }
    return nil
}
```

- Type switches: concrete cases before interface cases (a later concrete case is unreachable); add `case nil`
  when nil is valid input, or it falls into `default`. Errors use `errors.As`, never a bare `err.(*T)`.
- Generics over `any` when the logic is identical across types; behavior that differs per type is an interface.
  A generic body with a type switch inside is an interface in disguise.

### Test seams in this harness

The seam for a real boundary is a package var swapped in the test, not an interface invented for mocking
([testing-guidelines.md](../../../rules/standards/testing-guidelines.md) → A test must guard). More than two mocks
signals a missing seam, not a missing interface.

## Structs and embedding

| Choose | When |
| --- | --- |
| Embedded field | The outer type genuinely *is* the inner one plus more, and every promoted method is wanted |
| Named field | The outer type *uses* the inner one; expose only what you delegate explicitly |

Embedding pitfalls:

- **API leak.** Every exported method of the embedded type becomes yours; embedding `sync.Mutex` publishes
  `Lock`/`Unlock` to callers. Embed a lock as a named, unexported field.
- **Receiver is the inner value.** A promoted method cannot see the outer struct; "overriding" only shadows at the
  outer level, and the inner type's own calls still hit its own method. There is no virtual dispatch.
- **Silent interface satisfaction.** An embedded interface makes the outer type satisfy it even if a method is
  never set; calling it on a nil embedded value panics at runtime, not at compile time.
- **Ambiguous selectors.** Two embedded types with the same method name compile until something calls it.
- **Wire-format churn.** Embedded structs flatten into JSON; a field added upstream appears in your output.

Other struct rules:

- Receivers: pointer when the method mutates, the struct holds a lock or is large; pick one kind per type.
- Never copy a struct holding a mutex or channel; `go vet` (`copylocks`) catches value copies.
- Tag every exported field of a serialized type; an untagged rename changes the wire format.
- Enums start with an `Unknown` (or unset) member at `iota` zero so the zero value never passes as a real state.
- Make illegal states unrepresentable: an unexported field behind a validating constructor (`NewEmail`) beats a
  raw `string` checked at every layer. Validate at the boundary once, then trust internal data.

## Zero value and construction

- **Usable zero value first.** `bytes.Buffer`, `sync.Mutex` need no constructor; aim for the same. A nil map
  field panics on write, so allocate lazily in the method that writes, or require the constructor.
- **No constructor when the zero value works.** Add `NewX` only to enforce an invariant, take required
  dependencies, or return an error.
- **Return a new value, never mutate an argument** ([quality.md](../../../rules/standards/quality.md) → Rules).
  Mutating the struct under construction inside `NewX` is fine; mutating a caller's config is not.
- **No `init()` for setup, no mutable package globals.** `init()` cannot return an error, runs in file-name
  order before tests, and hides the dependency from every signature. Compile-once values
  (`regexp.MustCompile`, `//go:embed`) at package level are fine: immutable, and they fail at startup.
- Panic only for programmer bugs and `Must*` helpers at init; everything a caller can handle is a returned error.

### Options: plain parameters, config struct, or functional options

| Shape | Use when |
| --- | --- |
| Positional parameters | Two or three required inputs, no optional ones: the default |
| Config struct (`Config{...}`) | Several settings, mostly internal callers; a zero field means "use the default" |
| Functional options (`...Option`) | Published API, many optional knobs, must grow without breaking callers |

- Simplicity first: no options for a knob nobody turns. Inside `internal/` a config struct or one more parameter
  is the default; functional options there are usually speculative flexibility.
- When options can be invalid, make `Option` return an error and fail in the constructor, not at first use.
- Defaults live in code (`Config{Port: 8080}` in the constructor), never in struct-tag magic or reflection.

```go
type Config struct {
    Addr    string
    Timeout time.Duration // zero means 5s
}

func NewServer(cfg Config, store Store, log *slog.Logger) (*Server, error) {
    if cfg.Addr == "" {
        return nil, errors.New("server: addr required")
    }
    if cfg.Timeout == 0 {
        cfg.Timeout = 5 * time.Second // cfg is a copy: the caller's value is untouched
    }
    return &Server{cfg: cfg, store: store, log: log}, nil
}
```

## Dependency injection

- **Hand-wire constructors at the composition root.** `main` (or the verb's `Run`) builds dependencies in order:
  config → infrastructure → stores → services → transport; each constructor takes what it needs as parameters.
- A constructor that opens its own DB, reads env vars or calls `sql.Open` is a hidden dependency: lift it out.
- Pass the specific dependency, never a registry or container (service locator); never a global.
- Keep the graph shallow. A five-deep chain means a service depends on another service where it needed the store.
- Close in reverse with `defer` right after a successful open; lifecycle stays visible in one function.
- Tests construct the unit directly with a fake or a swapped seam; no container needed.

### DI containers: when, and when not

wire (codegen), dig/fx (runtime reflection, lifecycle hooks) and do (generics, lazy, scopes) solve wiring at
dozens of services with shared lifecycle. Below that they add a second language for what plain calls already say,
move failures from compile time (or a readable `main`) into graph resolution, and spread the container's
imports. In this harness: not used, stdlib first; wiring stays explicit in `Run`. Revisit only when `main` wiring
itself is the measured pain, and propose it before adding the dependency.

## Package boundaries

- **A package is a unit of meaning, not a file bucket.** Name it for what it provides (`ledger`, `router`), so
  call sites read `ledger.Append`. No `util`, `common`, `helpers`, `misc`, `base`, `types`: they grow without
  bound, import everything and end up in every cycle. Put a helper in the package that uses it, or a narrowly
  named one (`textwrap`) when two areas truly share it.
- **Short, lowercase, singular, matching the directory**; no stutter (`config.Config` is fine, `config.ConfigLoader`
  is not).
- **Dependencies point inward.** Domain logic imports no transport or storage; adapters import the domain.
- **Placement by importer count** (the harness rule): a package more than one area imports stays top-level under
  `internal/`; one area's helper nests under that area (`internal/agent/tools`); a provider's code stays under
  that provider (`internal/databricks/oauth`).
- **Export the minimum.** Every exported name is a commitment; inside `internal/` it is still a cost to read.
- A file split (same package) often fixes a bloated package without touching its import graph; split the
  package only along a real boundary.

### Breaking import cycles, cheapest first

| # | Fix | When |
| --- | --- | --- |
| 1 | Consumer-side interface in the importing package | It calls a method or two; no call site changes |
| 2 | Extract the shared type into a leaf package both import | Both need the same concrete type |
| 3 | Shared code under a nested `internal/` | Siblings share detail that must stay private to the subtree |
| 4 | Mediator package both delegate to | 1-3 don't fit; rare |

Moving the producer's concrete type into the consumer usually relocates the cycle rather than removing it.

## Layout: cmd/ and internal/

| Path | Holds |
| --- | --- |
| `cmd/<binary>/main.go` | `package main`: parse, wire, call `Run`, exit with a code. No logic |
| `internal/<area>/` | All application code; the compiler forbids imports from outside the module root |
| `internal/cli/<verb>.go` (harness) | Flags and output only, plus `<verb>_test.go`; logic lives in the area's package |
| `testdata/` | Fixtures next to the package that reads them; ignored by the build |

- **No `pkg/`.** It signals nothing the compiler enforces and invites accidental public API. A binary's code
  belongs in `internal/`; a real library exposes packages at the module root, private parts under `internal/`.
- No `src/`, no `main.go` at the root of a multi-binary repo, no layer directories (`models/`, `services/`)
  before the code is big enough to need them. A flat package is correct for a small tool.
- Tests sit beside their source (`foo.go` + `foo_test.go`); a test belongs to the package whose code it tests.
- Module path matches the repo URL, lowercase.

## Refactoring safely

- **Structure and behavior never share a commit.** A rename is reviewed for "did anything change" and a fix for
  "is this right"; mixing them forces both reviews on every line. Move and optimize are two commits too.
- **Safety net first.** Scoped tests over the blast radius pass before the first edit; untested code gets a
  characterization test that pins current output. A change that breaks only tests means rewriting those tests
  against behavior, not preserving fixtures for states the code can no longer produce.
- **Map the blast radius** before editing: gopls references and call hierarchy, plus `grep` for the name.
- **Tools over hand edits.** `gopls rename` refuses shadowing and broken interface satisfaction; `gofmt -r`, `eg`
  or a `go/analysis` fixer for repeated rewrites. Hand-editing dozens of sites misses some.
- **Then grep for what the compiler can't see**: struct tags, `text/template` fields, `reflect` lookups, string
  names in JSON, docs and help text.
- **Verify every step**: `go build ./... && go vet ./...`, the touched packages' tests, `-race` for concurrency,
  `benchstat` for a hot path (anything but `~` is a behavior change).
- **Commit at every green step; a red step goes back**, not forward. In the live `~/.claude` checkout never
  `reset`/`checkout`/`stash`/`restore`: undo with a new commit or `git revert`, or do the work in a linked
  worktree ([worktree-git.md](../../../rules/standards/worktree-git.md)).
- **Skip it** when nothing planned will touch the code, the purpose is unclear, or a deadline would rush review.

### Internal clean break vs published deprecation

| Code | How a rename or move lands |
| --- | --- |
| `internal/`, this module only | One pass: move or rename, fix every caller, delete the old name; no alias |
| Published API (importable by others) | Add new, mark old `// Deprecated: use X`, alias `type Old = pkg.New` |

Type aliases (`type A = B`, with the `=`) exist for gradual repair across module boundaries. Inside one module
the compiler lists every caller, so an alias is exactly the back-compat shim
[quality.md](../../../rules/standards/quality.md) forbids. Breaking a published API means a `/vN` major version.

Adapted from samber/cc-skills-golang (MIT): golang-structs-interfaces, golang-design-patterns,
golang-dependency-injection, golang-refactoring, golang-project-layout.
