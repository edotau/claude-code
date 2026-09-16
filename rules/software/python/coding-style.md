---
paths:
  - "**/*.py"
  - "**/*.pyi"
---
# Python Coding Style

> This file extends [standards/quality.md](../../standards/quality.md) with Python specific content.

## Standards

- Follow **PEP 8** conventions
- Use **type annotations** on all function signatures

## Naming — No Leading Underscore

Do **not** prefix functions, constants, module-level names, or variables with `_`. The underscore convention encodes an implementation-private hint the import system cannot enforce; this project prefers the Pythonic alternative of simply not exporting via `__all__`.

```python
# WRONG
_HOST_FALLBACK = ""
def _mask_env_keys(keys): ...
def _build_strategies(self, ...): ...

# RIGHT
HOST_FALLBACK = ""
def mask_env_keys(keys): ...
def build_strategies(self, ...): ...  # class method — no underscore either
```

**If you believe there is a strong reason to use a leading underscore** (e.g., avoiding a real, concrete name collision with a public API that cannot be renamed), stop and **ask the user before writing the name**. Do not retroactively justify the underscore after typing it.

## Immutability

Prefer immutable data structures:

```python
from dataclasses import dataclass

@dataclass(frozen=True)
class User:
    name: str
    email: str

from typing import NamedTuple

class Point(NamedTuple):
    x: float
    y: float
```

## Formatting

**ruff is the entire toolchain** (black/isort/autoflake retired in its favor):

- `ruff check --fix` — lint + auto-fix: unused imports/variables (F401/F841, ⇐ autoflake)
  and import sorting (I rules, ⇐ isort)
- `ruff format` — code formatting (black-compatible output, 120-col lines)

## Comments & Docstrings — Short and Concise

Keep every comment and docstring to **120 characters or 2 lines, whichever comes first**.
A comment that needs more than two lines is a signal the code (or the comment) should be
restructured, not expanded.

- State the *why*, not the *what* — the code already says what it does.
- One concern per comment. If you need a paragraph, split the logic or extract a function.
- **Delete commented-out code.** Do not leave it as a comment "for later" — git history is
  the archive. If it must stay temporarily, it still obeys the 120-char / 2-line cap.
- **A function/class/method docstring holds a strict shape** — one-line description, then `Args:`,
  then `Return:`, and NOTHING else. **Never add a `Raises:` section**: what a function raises is the
  caller's contract and belongs in `rules/`, not restated per docstring where it goes stale silently.
  That shape is EXEMPT from the cap. Never drop a section or an `Args:` entry
  to hit a line target. The cap targets the *narrative paragraph* wrapped around those sections and
  the scattered `#` blocks between statements: reduce those to 1–2 lines, or delete them outright.
- Compressing a gotcha keeps the **constraint** and drops the story — no incident dates, no history,
  no retelling. If the constraint genuinely needs 2 lines, use 2; never delete it to reach 1.
- Logger and exception **message** strings are code, not comments — the cap does not license editing
  them, and tests often assert their text.
- A bulk trim is provable: hash each file's AST with docstrings stripped (`ast.dump` of the tree with
  leading string `Expr`s removed) before and after. Identical hashes mean no code moved — that gate
  is exhaustive where a test suite only catches what it happens to cover.

```python
# WRONG — multi-line essay explaining mechanics the code already shows
# This function iterates over every item in the input list, and for each one it
# looks up the score from the cache, and if the cache misses it falls
# back to recomputing the value from scratch using the model...
def score(items): ...

# RIGHT — one line, says WHY
# Cache-first: recompute only misses (model call is ~200ms/item).
def score(items): ...
```
