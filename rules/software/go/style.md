---
paths:
  - "**/*.go"
---

# Go Style

> This file extends [quality.md](../../standards/quality.md) with the editor (gopls) warnings `make lint` misses.

**Never concatenate inside `WriteString` on a `strings.Builder`, `bytes.Buffer` or `bufio.Writer`.** `a + b` builds a
throwaway string before the copy; gopls reports it as `writestring: Inefficient string concatenation in call to
WriteString`. Write one call per piece. `golangci-lint` does not run this check, so the editor is the only signal.

```go
b.WriteString(prefix + name + "\n") // WRONG: allocates the joined string first

b.WriteString(prefix) // RIGHT
b.WriteString(name)
b.WriteString("\n")
```

- Constants joined at compile time are fine as one call: `b.WriteString(header + "\n")` with a `const header`.
- **A file, pipe or socket keeps the single call** (`f.WriteString(key + "\n")`, `io.WriteString(w, s+"\n")`): there
  each call is its own write, so splitting it costs a system call and lets another writer land between the pieces.
- With formatting verbs, use `fmt.Fprintf(&b, ...)`, never `b.WriteString(fmt.Sprintf(...))`.
