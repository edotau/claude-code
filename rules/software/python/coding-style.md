---
paths:
  - "**/*.py"
  - "**/*.pyi"
---
# Python Coding Style

> This file extends [standards/quality.md](../../standards/quality.md) with Python specific content.

- Tooling is **ruff** alone (`ruff check --fix`, `ruff format`, 120 columns); type annotations on every function
  signature; prefer `@dataclass(frozen=True)` or `typing.NamedTuple`.
- Comments follow the cap in `quality.md`; delete commented-out code (git is the archive). Logger and exception
  message strings are code: tests assert their text, so a comment trim never edits them.
- A docstring is a one-line description, then `Args:`, then `Return:`, nothing else; the cap does not apply to that
  shape, and no `Args:` entry is dropped to fit it. Never add `Raises:`: what a function raises is the caller's
  contract and goes stale per docstring.
- Compressing a gotcha keeps the constraint and drops the story (no dates, no history).

## Naming — No Leading Underscore

Never prefix functions, constants, module-level names or variables with `_`; the import system cannot enforce it, so
control exports with `__all__`. If a real name collision with a public API seems to demand one, ask the user before
writing the name.

```python
# WRONG
_HOST_FALLBACK = ""
def _mask_env_keys(keys): ...

# RIGHT
HOST_FALLBACK = ""
def mask_env_keys(keys): ...
```
