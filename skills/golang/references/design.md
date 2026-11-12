# Go Design and Layout

Types, interfaces, constructors, dependency wiring, package boundaries and safe restructuring.

## Interfaces

- **The consumer owns the interface**, naming only the methods it calls; the producer exports a concrete type and
  never imports the interface. A concrete return keeps every method visible and the caller can narrow it.
- **Discover, don't design.** One interface, one implementation, no fake = pure indirection: delete it. An interface
  earns its place from a second production implementation, not from a test; for a single real boundary (clock,
  network, exec) a package-level func var swapped in the test is often enough. Many mocks signal a missing seam or a
  function doing too much.
- **Compile-time check** only where satisfaction is the point and nothing else would catch a break:
  `var _ io.Writer = (*Sink)(nil)` next to the type.
- **Optional capability via assertion**: keep the parameter minimal and upgrade at runtime with comma-ok, asserting
  to a behavior (`w.(interface{ Flush() error })`), never to a concrete type like `*os.File`.
- Type switches: concrete cases before interface cases (a later concrete case is unreachable); add `case nil`
  when nil is valid input, or it falls into `default`. Errors use `errors.As`, never a bare `err.(*T)`.
- A generic body with a type switch inside is an interface in disguise: behavior that differs per type is an
  interface; identical logic across types is a generic.

## Embedding

| Choose | When |
| --- | --- |
| Embedded field | The outer type genuinely *is* the inner one plus more, and every promoted method is wanted |
| Named field | The outer type *uses* the inner one; expose only what you delegate explicitly |

- **API leak.** Every exported method of the embedded type becomes yours; embedding `sync.Mutex` publishes
  `Lock`/`Unlock` to callers. Embed a lock as a named, unexported field.
- **Receiver is the inner value.** A promoted method cannot see the outer struct; "overriding" only shadows at the
  outer level, and the inner type's own calls still hit its own method. There is no virtual dispatch.
- **Silent interface satisfaction.** An embedded interface makes the outer type satisfy it even if a method is
  never set; calling it on a nil embedded value panics at runtime, not at compile time.
- **Ambiguous selectors.** Two embedded types with the same method name compile until something calls it.
- **Wire-format churn.** Embedded structs flatten into JSON; a field added upstream appears in your output.

## Structs and construction

- Never copy a struct holding a mutex or channel; `go vet` (`copylocks`) catches value copies.
- Tag every exported field of a serialized type; an untagged rename changes the wire format.
- Make illegal states unrepresentable: an unexported field behind a validating constructor (`NewEmail`) beats a
  raw `string` checked at every layer.
- A nil map field panics on write: allocate lazily in the method that writes, or require the constructor. Add `NewX`
  only to enforce an invariant, take required dependencies, or return an error.
- Don't mutate a config or argument the caller still owns; mutating the struct under construction is fine.
- **No `init()` for setup, no mutable package globals.** `init()` cannot return an error, runs in file-name
  order before tests, and hides the dependency from every signature. Compile-once values
  (`regexp.MustCompile`, `//go:embed`) at package level are fine: immutable, and they fail at startup.

### Plain parameters, config struct, or functional options

| Shape | Use when |
| --- | --- |
| Positional parameters | Two or three required inputs, no optional ones: the default |
| Config struct (`Config{...}`) | Several settings, mostly internal callers; a zero field means "use the default" |
| Functional options (`...Option`) | Published API, many optional knobs, must grow without breaking callers |

- Inside `internal/`, functional options are usually speculative flexibility.
- When options can be invalid, make `Option` return an error and fail in the constructor, not at first use.
- Defaults live in code (applied in the constructor), never in struct-tag magic or reflection.

## Dependency injection

- Hand-wire constructors at the composition root (`main`/`run`): config → infrastructure → stores → services →
  transport. A constructor that opens its own DB or reads env vars is a hidden dependency: lift it out.
- Pass the specific dependency, never a registry or container (service locator). A five-deep chain means a service
  depends on another service where it needed the store.
- Close in reverse with `defer` right after a successful open; lifecycle stays visible in one function.
- Containers — wire (codegen), dig/fx (runtime reflection, lifecycle hooks), do (generics, lazy, scopes) — pay off
  only at dozens of services with shared lifecycle. Below that they move failures from compile time into graph
  resolution; reach for one only when explicit wiring itself is the measured pain.

## Package boundaries

- Naming rules: [style.md](style.md) → Naming. Put a helper in the package that uses it, or a narrowly named one
  (`textwrap`) when two areas truly share it.
- **Dependencies point inward.** Domain logic imports no transport or storage; adapters import the domain.
- **Place by importer count:** a package several areas import stays top-level under `internal/`; a helper only one
  area uses nests under that area (`internal/billing/tax`), so its scope is visible in the path.
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

## Layout

| Path | Holds |
| --- | --- |
| `cmd/<binary>/main.go` | `package main`: parse, wire, call `Run`, exit with a code. No logic |
| `internal/<area>/` | All application code; the compiler forbids imports from outside the module root |
| `testdata/` | Fixtures next to the package that reads them; ignored by the build |

- **No `pkg/`.** It signals nothing the compiler enforces and invites accidental public API. A real library exposes
  packages at the module root, private parts under `internal/`.
- No `src/`, no `main.go` at the root of a multi-binary repo, no layer directories (`models/`, `services/`)
  before the code needs them. A flat package is correct for a small tool.

## Refactoring safely

- **Structure and behavior never share a commit**: a rename is reviewed for "did anything change", a fix for "is
  this right". Move and optimize are two commits too.
- Untested code gets a characterization test that pins current output before the first edit.
- **Tools over hand edits.** `gopls rename` refuses shadowing and broken interface satisfaction; `gofmt -r`, `eg`
  or a `go/analysis` fixer for repeated rewrites. Call hierarchy before editing: [tooling.md](tooling.md).
- **Then grep for what the compiler can't see**: struct tags, `text/template` fields, `reflect` lookups, string
  names in JSON, docs and help text.
- Verify each step with `go build ./... && go vet ./...`, the touched packages' tests, `-race` for concurrency,
  `benchstat` for a hot path (anything but `~` is a behavior change). A red step goes back to the last green commit.

### Internal clean break vs published deprecation

| Code | How a rename or move lands |
| --- | --- |
| `internal/`, this module only | One pass: move or rename, fix every caller, delete the old name; no alias |
| Published API (importable by others) | Add new, mark old `// Deprecated: use X`, alias `type Old = pkg.New` |

Type aliases (`type A = B`) exist for gradual repair across module boundaries; inside one module the compiler lists
every caller, so an alias is a shim nobody needs. Breaking a published API means a `/vN` major version.
