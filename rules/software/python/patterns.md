---
paths:
  - "**/*.py"
  - "**/*.pyi"
---
# Python Patterns

> This file extends [standards/quality.md](../../standards/quality.md) with Python specific content.

## Protocol (Duck Typing)

```python
from typing import Protocol

class Repository(Protocol):
    def find_by_id(self, id: str) -> dict | None: ...
    def save(self, entity: dict) -> dict: ...
```

## Dataclasses as DTOs

```python
from dataclasses import dataclass

@dataclass
class CreateUserRequest:
    name: str
    email: str
    age: int | None = None
```

## Context Managers & Generators

- Use context managers (`with` statement) for resource management
- Use generators for lazy evaluation and memory-efficient iteration

## Optional Deps and TYPE_CHECKING Names

- A name imported only under `if TYPE_CHECKING:` is legal **in annotations only**. `cast()` evaluates its
  first argument at runtime, so it needs the quoted form: `cast("V1Secret", ...)`.
- `module.Attr` is not a valid type expression when `module` is a *variable* (e.g. an absent-SDK stub
  instance). Import the type itself instead of annotating through the variable.
- **Never blanket-replace a type name across a file.** `client.CoreV1Api` -> `CoreV1Api` also rewrote a
  runtime constructor call and turned 154 tests into `NameError`. Restrict such rewrites to annotation
  positions, then grep for `Name(` call sites before running the suite.

## Never Mutate `os.environ` to Steer a Library

`os.environ.pop(...)` + restore-in-`finally` is **process-global**: a sibling thread, a thread pool, or an
xdist worker sees the variable missing for the whole window. Two shapes replace it, and one of them almost
always exists:

- **Pin the library's own precedence.** Most cloud SDKs resolve an explicit constructor kwarg (host,
  credentials, auth type) BEFORE the env var, so passing it makes the ambient env irrelevant and nothing
  needs removing. Read the library's resolution order before assuming a scrub is required.
- **Scrub a COPY for a child process.** `env = {k: v for k, v in os.environ.items() if k not in NAMES}` then
  hand it to `subprocess`. In shell, `env -u VAR cmd` for the child only — never a bare `unset` in a
  sourced file, which mutates the caller's shell.

Keep such a name list to the vars the project ACTUALLY sets. Padding it with another cloud's vars (Azure/GCP
on an AWS deployment) makes it unauditable; grep each candidate before adding it.

