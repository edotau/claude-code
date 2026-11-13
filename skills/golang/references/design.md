# Go Design and Layout

Types, interfaces, constructors, dependency wiring, package boundaries and safe restructuring.

## Interfaces

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

## Embedding

| Choose | When |
| --- | --- |
| Embedded field | The outer type *is* the inner one plus more, and every promoted method is wanted |
| Named field | The outer type *uses* the inner one; expose only what you delegate |

- **API leak.** Every exported method of the embedded type becomes yours (`sync.Mutex` publishes `Lock`/`Unlock`);
  embed a lock as a named, unexported field.
- **Receiver is the inner value.** A promoted method can't see the outer struct; "overriding" only shadows, the inner
  type's own calls hit its own method. No virtual dispatch.
- **Silent interface satisfaction.** An embedded interface satisfies the outer type even if never set; calling it on
  nil panics at runtime.
- **Ambiguous selectors.** Two embedded types with the same method name compile until something calls it.
- **Wire-format churn.** Embedded structs flatten into JSON; an upstream field addition shows up in your output.

## Structs and construction

- Never copy a struct holding a mutex or channel (`go vet` copylocks).
- Tag every exported field of a serialized type; an untagged rename changes the wire format.
- Make illegal states unrepresentable: an unexported field behind a validating constructor (`NewEmail`) beats a raw
  `string` checked everywhere.
- A nil map field panics on write: allocate in the writing method or the constructor. `NewX` only to enforce an
  invariant, take required dependencies, or return an error.
- Don't mutate a config or argument the caller still owns.
- **No `init()` for setup, no mutable package globals**: `init()` can't return an error, runs in file-name order
  before tests, and hides dependencies. Compile-once values (`regexp.MustCompile`, `//go:embed`) are fine.

### Plain parameters, config struct, or functional options

| Shape | Use when |
| --- | --- |
| Positional parameters | Two or three required inputs, none optional: the default |
| Config struct (`Config{...}`) | Several settings, mostly internal callers; zero field = default |
| Functional options (`...Option`) | Published API, many optional knobs, must grow without breaking callers |

- Inside `internal/`, functional options are usually speculative.
- When options can be invalid, `Option` returns an error and the constructor fails, not first use.
- Defaults live in code (constructor), never struct-tag magic or reflection.

## Dependency injection

- Hand-wire at the composition root (`main`/`run`): config → infrastructure → stores → services → transport. A
  constructor that opens its own DB or reads env vars hides a dependency: lift it out.
- Pass the specific dependency, never a registry or container. A five-deep chain means a service depends on another
  service where it needed the store.
- `defer` close in reverse right after a successful open.
- Containers (wire codegen; dig/fx reflection; do generics) pay off only at dozens of services with shared lifecycle;
  below that they move failures from compile time into graph resolution.

## Package boundaries

- Naming: [style.md](style.md) → Naming. Put a helper in the package that uses it, or a narrowly named one
  (`textwrap`) when two areas share it.
- **Dependencies point inward.** Domain imports no transport or storage; adapters import the domain.
- **Place by importer count:** several importers → top-level under `internal/`; one area → nested under it
  (`internal/billing/tax`).
- A file split often fixes a bloated package; split the package only along a real boundary.

### Breaking import cycles, cheapest first

| # | Fix | When |
| --- | --- | --- |
| 1 | Consumer-side interface in the importing package | It calls a method or two; no call site changes |
| 2 | Shared type into a leaf package both import | Both need the same concrete type |
| 3 | Shared code under a nested `internal/` | Siblings share detail private to the subtree |
| 4 | Mediator package both delegate to | 1-3 don't fit; rare |

Moving the producer's concrete type into the consumer usually relocates the cycle.

## Layout

| Path | Holds |
| --- | --- |
| `cmd/<binary>/main.go` | `package main`: parse, wire, call `Run`, exit with a code. No logic |
| `internal/<area>/` | All application code; compiler forbids imports from outside the module root |
| `testdata/` | Fixtures next to the package that reads them; ignored by the build |

- **No `pkg/`**: it enforces nothing and invites accidental public API. A real library exposes packages at the module
  root, private parts under `internal/`.
- No `src/`, no root `main.go` in a multi-binary repo, no layer directories (`models/`, `services/`) before needed.
  A flat package is right for a small tool.

## Refactoring safely

- **Structure and behavior never share a commit**; move and optimize are two commits too.
- Untested code gets a characterization test pinning current output before the first edit.
- **Tools over hand edits**: `gopls rename` refuses shadowing and broken interface satisfaction; `gofmt -r`, `eg` or
  a `go/analysis` fixer for repeated rewrites. Call hierarchy first: [tooling.md](tooling.md).
- **Then grep for what the compiler can't see**: struct tags, `text/template` fields, `reflect` lookups, JSON string
  names, docs and help text.
- Verify each step: `go build ./... && go vet ./...`, touched packages' tests, `-race` for concurrency, `benchstat`
  for hot paths (anything but `~` is a behavior change). Red step → back to the last green commit.

### Internal clean break vs published deprecation

| Code | How a rename or move lands |
| --- | --- |
| `internal/`, this module only | One pass: rename, fix every caller, delete the old name; no alias |
| Published API | Add new, mark old `// Deprecated: use X`, alias `type Old = pkg.New` |

Aliases (`type A = B`) are for gradual repair across module boundaries; inside one module they're a needless shim.
Breaking a published API means a `/vN` major version.
