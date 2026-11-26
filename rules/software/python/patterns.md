---
paths:
  - "**/*.py"
  - "**/*.pyi"
---
# Python Patterns

Extends [quality.md](../../standards/quality.md).

## Optional Deps and TYPE_CHECKING Names

- Names imported under `if TYPE_CHECKING:` are annotation-only; quote runtime `cast()` types: `cast("V1Secret", ...)`.
- An absent-SDK stub variable's `module.Attr` is not a type expression; import the type.
- Restrict type-name replacements to annotations, then search `Name(` constructor calls before testing.

## Never Mutate `os.environ` to Steer a Library

Pop-and-restore changes process-global state visible to sibling threads and xdist workers.

- Read the library's precedence; prefer explicit constructor host, credentials or auth type over ambient env.
- Otherwise pass a scrubbed copy to the child: `env = {k: v for k, v in os.environ.items() if k not in NAMES}`.
  In shell use `env -u VAR cmd`, never bare `unset` in a sourced file.
- Search each candidate first; list only variables the project sets.
