# Python Idiom Catalog

Deep reference for [../SKILL.md](../SKILL.md). Load when you need a specific pattern.
All examples follow the harness rules in SKILL.md — notably `%`-style logging and modern
type-hint syntax.

## Core principles

- **Readability counts** — code should be obvious. Prefer clarity over cleverness.
- **Explicit > implicit** — avoid hidden side effects and `import *`.
- **EAFP > LBYL** — try the operation and handle the exception, rather than pre-checking,
  when the happy path normally succeeds.

```python
# EAFP — preferred
try:
    return dictionary[key]
except KeyError:
    return default
```

## Context managers

Use `with` for all resource management — never manual `open()`/`close()` in a `try/finally`.

Custom context managers via `contextlib`:

```python
from contextlib import contextmanager
import time

@contextmanager
def timer(name: str):
    start = time.perf_counter()
    yield
    logger.info("%s took %.4fs", name, time.perf_counter() - start)
```

Class form for transactional resources — commit on clean exit, roll back on exception, and
return `False` so exceptions propagate:

```python
class DatabaseTransaction:
    def __init__(self, connection):
        self.connection = connection

    def __enter__(self):
        self.connection.begin_transaction()
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        if exc_type is None:
            self.connection.commit()
        else:
            self.connection.rollback()
        return False  # don't suppress exceptions
```

## Custom exception hierarchies

```python
class AppError(Exception):
    """Base for all application errors."""

class ValidationError(AppError):
    """Input validation failed."""

class NotFoundError(AppError):
    """Requested resource not found."""
```

A single base lets callers catch `AppError` broadly while specific sites raise precise types.

## Comprehensions & generators

Use a comprehension for simple transformations; expand anything with multiple conditions
into a named function.

```python
names = [u.name for u in users if u.is_active]          # good
total = sum(x * x for x in range(1_000_000))            # generator — no intermediate list
```

Generator functions stream large data without holding it in memory:

```python
from collections.abc import Iterator

def read_lines(path: str) -> Iterator[str]:
    with open(path) as f:
        for line in f:
            yield line.strip()
```

## Decorators

Always `functools.wraps` to preserve metadata.

```python
import functools, time
from collections.abc import Callable

def timer(func: Callable) -> Callable:
    @functools.wraps(func)
    def wrapper(*args, **kwargs):
        start = time.perf_counter()
        result = func(*args, **kwargs)
        logger.info("%s took %.4fs", func.__name__, time.perf_counter() - start)
        return result
    return wrapper
```

Parameterized decorators add one wrapping layer (`def repeat(times): def decorator(func): ...`).
Class-based decorators (`__init__` + `__call__`) are useful when the decorator holds state.

## Useful patterns

### Tri-state "tried" flag

Three-valued flag (`None` = untried, `True` = ok, `False` = tried-and-failed) when an
operation may fail once and should not be retried the same way:

```python
class Client:
    verified_ssl: bool | None = None
```

Clearer than a bool because "untried" and "tried-and-failed" are distinct states.

### Idempotent-by-default

Write operations re-runnable without side effects — check for the result before doing work:

```python
from pathlib import Path

def ensure_output(path: Path) -> None:
    if path.exists() and path.stat().st_size > 0:
        return  # already done
    do_the_work(path)
```

Applies to filesystem work, Celery tasks, Jenkins steps, migrations.

## Concurrency

Match the tool to the bottleneck:

- **I/O-bound** → `ThreadPoolExecutor` (or `asyncio` for many concurrent awaits)
- **CPU-bound** → `ProcessPoolExecutor` (sidesteps the GIL)

```python
import concurrent.futures

def fetch_all(urls: list[str]) -> dict[str, str]:
    with concurrent.futures.ThreadPoolExecutor(max_workers=10) as ex:
        futures = {ex.submit(fetch_url, u): u for u in urls}
        out = {}
        for fut in concurrent.futures.as_completed(futures):
            url = futures[fut]
            try:
                out[url] = fut.result()
            except Exception as e:
                logger.error("fetch %s failed: %s", url, e)
                out[url] = ""
    return out
```

For async, gather with `return_exceptions=True` so one failure doesn't cancel the batch.

## Performance & memory

- `__slots__` on hot, numerous classes drops per-instance `__dict__` overhead.
- Stream large files with a generator instead of returning a full list.
- Never build strings with `+=` in a loop (O(n²)); use `"".join(...)` or `io.StringIO`.

## Package organization

`src/` layout, tests mirroring source:

```text
myproject/
├── src/mypackage/{__init__.py,main.py,api/,models/,utils/}
├── tests/{conftest.py,test_*.py}
└── pyproject.toml
```

Import order: stdlib, third-party, local (let isort enforce it). Control package exports
with `__all__` in `__init__.py`.

## Anti-patterns to avoid

```python
# Mutable default argument — shared across calls
def append_to(item, items=[]):       # WRONG
def append_to(item, items=None):     # RIGHT — create inside if items is None

@dataclass
class Basket:
    items: list = []                 # WRONG — same list shared by every instance
    items: list = field(default_factory=list)  # RIGHT

if type(obj) == list:                # WRONG
if isinstance(obj, list):            # RIGHT

if value == None:                    # WRONG
if value is None:                    # RIGHT

from os.path import *                # WRONG — namespace pollution
from os.path import join, exists     # RIGHT

logger.error(f"failed: {e}")         # WRONG — f-string in logger
logger.error("failed: %s", e)        # RIGHT — %-style, deferred interpolation
```

**Remember:** readable, explicit, least surprise. Clarity over cleverness.
