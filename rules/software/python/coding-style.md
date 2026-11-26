---
paths:
  - "**/*.py"
  - "**/*.pyi"
---
# Python Coding Style

Extends [quality.md](../../standards/quality.md).

- Use ruff (`ruff check --fix`, `ruff format`, 120 columns); annotate every function signature.
  Prefer `@dataclass(frozen=True)` or `typing.NamedTuple`.
- Follow the comment cap; delete commented-out code. Keep gotcha constraints, drop their history.
  Logger and exception strings are code, not comments to trim.
- Docstrings: one-line description, then `Args:` and `Return:`, nothing else. This shape is exempt from the cap;
  keep every argument entry. Never add `Raises:`.

## Naming — No Leading Underscore

Never prefix functions, constants, module-level names or variables with `_`; control exports with `__all__`.
Ask before using a leading underscore to resolve a public API name collision.
