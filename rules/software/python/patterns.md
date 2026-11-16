---
paths:
  - "**/*.py"
  - "**/*.pyi"
---
# Python Patterns

> This file extends [standards/quality.md](../../standards/quality.md) with Python specific content.

## Optional Deps and TYPE_CHECKING Names

- A name imported only under `if TYPE_CHECKING:` is legal in annotations only; `cast()` evaluates its first argument
  at runtime, so quote it: `cast("V1Secret", ...)`.
- `module.Attr` is not a valid type expression when `module` is a variable (an absent-SDK stub); import the type.
- Never blanket-replace a type name across a file (it also rewrites runtime constructor calls): restrict to
  annotation positions, then grep `Name(` call sites before running the suite.

## Never Mutate `os.environ` to Steer a Library

A pop-and-restore of `os.environ` is process-global: sibling threads and xdist workers see the variable missing.

- Pin the library's own precedence: most cloud SDKs resolve an explicit constructor kwarg (host, credentials, auth
  type) before the env var, so the ambient env stops mattering. Read the resolution order first.
- Otherwise scrub a copy for the child: `env = {k: v for k, v in os.environ.items() if k not in NAMES}` into
  `subprocess`; in shell `env -u VAR cmd`, never a bare `unset` in a sourced file.
- List only the vars the project actually sets; grep each candidate before adding it.
