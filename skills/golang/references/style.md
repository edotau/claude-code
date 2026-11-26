# Go Style and Design

Apply to the task's code, not an untouched-code sweep. Harness layout, comments and lint:
[rules/harness/go.md](../../../rules/harness/go.md); scope and naming:
[principles.md](../../../rules/standards/principles.md).

## Naming

- Keep receiver names consistent; boolean predicates use `Is`/`Has`/`Can`; enums have an `Unknown` zero or start at 1.
- Name conditions with three or more operands; leave expensive calls last and inline to preserve short-circuiting.
- Use `net.JoinHostPort` for addresses; string formatting breaks IPv6.

## Strings

Never concatenate inside `WriteString` on `strings.Builder`, `bytes.Buffer` or `bufio.Writer`: write each piece
separately. gopls reports `writestring`; golangci-lint does not. Compile-time constant concatenation is fine.

A file, pipe or socket keeps one write: splitting adds syscalls and permits interleaving. For formatting, use
`fmt.Fprintf(&b, ...)`, not `b.WriteString(fmt.Sprintf(...))`.

## Interfaces and embedding

- The consumer owns the interface. One implementation and no production alternative usually needs no interface;
  test seams belong to [testing-guidelines.md](../../../rules/standards/testing-guidelines.md#a-test-must-guard).
- Assert optional behavior, not a concrete type. In type switches put concrete cases before interfaces that match them.
- A generic body switching on types wants an interface; identical logic across types suits generics.
- Embed only when every promoted method belongs in the outer API. Named fields avoid leaking `Lock`/`Unlock`,
  accidental interface satisfaction and upstream JSON fields. A promoted method's receiver is the inner value;
  shadowing it is not virtual dispatch. An unset embedded interface can satisfy a contract and then panic on use.

## Structs, construction, wiring

- Tag exported serialized fields so renames do not silently change the wire format.
- Constructors enforce invariants or take required dependencies; defaults live in code, not reflection or tags.
- No setup in `init()` or mutable package globals; compile-once values are fine, as are the test seams linked above.
- Prefer positional required inputs, then a config struct for several settings. Functional options suit a published
  API that must grow; inside `internal/` they are usually speculative. Reject invalid options in the constructor.
- Pass specific dependencies, not registries. A constructor opening its own DB or reading env hides a dependency;
  lift it to the composition root. A deep service chain often needs the store directly.

## Package boundaries and layout

For general libraries, public packages live at the module root, private code under `internal/`; `pkg/` enforces
nothing. Keep domain code independent of transport/storage. Break cycles with a consumer interface, then a shared
leaf type or nested `internal/` helper; a mediator is the last resort. Moving a concrete type often moves the cycle.

## Refactoring safely

- Separate structural and behavior changes. For untested behavior follow the live-first and test-scope rules in
  [testing-guidelines.md](../../../rules/standards/testing-guidelines.md).
- Use `gopls rename` for symbol renames; then search tags, templates, reflection, JSON names, docs and help that the
  compiler cannot check. Build-configuration limits: [tools.md](tools.md#code-lookup-gopls-go-doc-pkggodev).
- Internal renames and published API compatibility follow [quality.md](../../../rules/standards/quality.md#rules).

## Modernize — old → new

Use the module's `go` directive, not the installed toolchain, to choose APIs. `modernize`/`go fix` can edit every
matching line in a package; use a mechanical sweep only when requested and inspect its whole diff. JSON v2 rejects
duplicate keys and invalid UTF-8: check real payloads before switching. `omitzero`, unlike `omitempty`, handles structs.

## golangci-lint v2

Harness configuration is owned by [go.md](../../../rules/harness/go.md#style-and-lint); suppression policy by
[quality.md](../../../rules/standards/quality.md#verification-order--lint-first-always).
