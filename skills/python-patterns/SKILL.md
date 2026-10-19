---
name: python-patterns
description: General Python conventions and idioms — logging, exceptions, type hints, dataclasses, context managers, concurrency, packaging, venv. Use when writing, reviewing, or refactoring Python code.
---

# Python Patterns

## Overview

Idiomatic, harness-aligned Python: readable, explicit, EAFP. This skill encodes
**general** rules; for anything project-variable, detect first and match what's there.

The deep idiom catalog (decorators, comprehensions, concurrency, `__slots__`,
anti-patterns) lives in [references/idioms.md](references/idioms.md) — load it when you
need a specific pattern. This page is the always-applicable core.

## When to Use

- Writing, reviewing, or refactoring any `.py` file
- Load alongside framework skills (`django`) when relevant
- Designing a package/module layout

## Detect-before-prescribe

For anything project-variable (test framework, formatter, line length, Python version),
**check the project config first** and match it. Look at:

- `pyproject.toml` — `[tool.pytest.ini_options]`, `[tool.black]`, `[tool.ruff]`,
  `[tool.isort]`, `requires-python`
- Existing tests — `unittest.TestCase` vs `pytest` fixtures
- Existing source — type-hint style already in use

When in doubt, ask rather than assume.

## Logging — `%`-style only

Always use `%`-style formatting in log calls. Never f-strings or `.format()` — string
interpolation must only happen when the log level is active.

```python
# Correct — interpolation deferred until the level is active
logger.info("Processing %s items for user %s", count, username)

# Wrong — f-string evaluates regardless of log level
logger.info(f"Processing {count} items for user {username}")
```

This holds even in `except` blocks: `logger.error("Operation failed: %s", e)`, never an
f-string. f-strings are fine *outside* logging.

## Exception handling

Catch specific exceptions; use `except Exception` only as a last-resort fallback. Chain
with `from` to preserve the traceback, and log details with `%s`:

```python
try:
    parsed = json.loads(data)
except json.JSONDecodeError as e:
    logger.debug("parse failed: %s", e)
    raise ValueError(f"invalid config: {path}") from e
```

Consolidate try/except blocks — never nest them. Group operations under one `try` with
narrow `except` clauses, or extract a function so the caller's try/except stays flat.
For custom error hierarchies and context-manager rollback, see references/idioms.md.

## Environment variables — guard on value, not presence

For a semantically boolean env var, `if "FLAG" in os.environ:` fires whenever the var is
*set to anything* — including falsey strings (`false`, `0`, `""`) that `.env` files and CI
routinely export. Guard on the value:

```python
# Wrong — PROD_ENVIRONMENT=false still enters the branch (present but false)
if "PROD_ENVIRONMENT" in os.environ:
    swap_in_prod_driver()

# Correct — only true-ish values enter
if os.environ.get("PROD_ENVIRONMENT", "").lower() == "true":
    swap_in_prod_driver()
```

Caveat: if the codebase already establishes *presence*-based semantics for that same var
(e.g. `DEBUG = "PROD_ENVIRONMENT" not in os.environ`), don't silently introduce a
value-based check elsewhere — two readings of one var is a bug farm. Flag the
inconsistency instead. For required secrets, prefer `os.environ["KEY"]` (fail-fast) over
`.get()` — see `rules/software/python/security.md`.

## Type hints — modern syntax

Use built-in generics and union syntax (Python 3.10+). Annotate return types on all
public functions.

```python
def find_user(user_id: int) -> dict[str, str] | None: ...
def get_ids() -> list[int]: ...
```

Check `requires-python`: only fall back to `typing.Optional/List/Dict` if the project
targets < 3.10. Mixing styles inside one module is worse than either consistently.
Prefer `typing.Protocol` for duck-typed interfaces over ABC inheritance.

## No new helper methods

When refactoring, do not introduce new private helper functions. Refactor within existing
boundaries. A too-long function is a signal to simplify the logic, not to split it into
helpers — helpers accumulate into a maze.

## Immutability & data containers

Prefer `@dataclass(frozen=True)` for value objects and `typing.NamedTuple` for immutable
tuples. Use `field(default_factory=...)` for mutable defaults — never a bare `[]`/`{}`
default argument (a classic shared-state bug; see references/idioms.md).

## Venv discipline

Always invoke Python through the venv explicitly, never bare `python`:

- Local: `.venv/bin/python`, `.venv/bin/pytest`, `.venv/bin/ruff`
- Containers: absolute path, e.g. `/opt/venv/bin/python`
- Scripts: set `PYTHON="${VENV}/bin/python"` and use `"${PYTHON}"`

Never rely on `PATH` resolution for the interpreter — it masks bugs when venvs are rebuilt
or a subprocess inherits a different environment.

## Packaging

`pyproject.toml` is the single config source — no `setup.py`, no `setup.cfg`.
`setuptools-scm` for git-tag versioning is common. Use `__all__` to control exports rather
than leading-underscore names.

## Testing

Detect the framework before writing tests:

- `[tool.pytest.ini_options]` → pytest
- `unittest.TestCase` in existing tests → unittest (`self.assertEqual`, never bare `assert`)

Tests mirror source layout (`src/foo/bar.py` → `tests/unit/foo/test_bar.py`). Fixtures in
`conftest.py`. Never run the full suite unprompted — ask first, especially for unknown
runtimes.

## Formatting

Defer to project config (`pyproject.toml`). Never reformat a whole file for a targeted
change — only touch the lines you are already changing.

## Quick Reference

| Idiom | Use for |
|-------|---------|
| EAFP (`try/except`) | Access that usually succeeds |
| Context managers (`with`) | Resource management |
| List comprehension | Simple transformations |
| Generators (`yield`) | Lazy evaluation / large data |
| `@dataclass(frozen=True)` | Immutable value objects |
| `Protocol` | Duck-typed interfaces |
| `__slots__` | Memory-tight hot classes |
| `"".join(...)` | String building (never `+=` in a loop) |
| `is None` / `isinstance` | Identity / type checks (never `== None`, `type() ==`) |

## Guardrails

Never do without explicit user confirmation:

- `rm -rf` on anything under the repo
- `pip install` to the system interpreter (always use venv)
- `pytest` / `unittest` without a runtime estimate
- Reformatting a file larger than the user asked about
- Changing type-hint style across a module when asked about one function

## Additional Resources

- [references/idioms.md](references/idioms.md) — full catalog: context managers, decorators,
  comprehensions/generators, concurrency, performance, anti-patterns
- `lib/check-style.sh` — runs ruff + black --check on target files
