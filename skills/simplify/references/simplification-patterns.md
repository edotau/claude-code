# Simplification patterns

The recurring wins when reducing complexity. Each is behavior-preserving — you change how
the code reads, not what it does.
Detector tags and review severity: `skills/evaluate-code/references/principles.md` (#2).

## Inline single-use abstractions

A class that wraps one method, or a helper called from one place, is indirection without
payoff. Inline it; add the abstraction back when a second caller actually appears.

`review complexity` → `premature-abstraction`, `class-density` — a factory for one product is the same case: call it.

**Bad — Strategy/ABC scaffolding for one discount type (150 lines):**

```python
class DiscountStrategy(ABC):
    @abstractmethod
    def calculate(self, amount: float) -> float: ...

class PercentageDiscount(DiscountStrategy): ...

@dataclass
class DiscountConfig:
    strategy: DiscountStrategy
    min_purchase: float = 0.0

class DiscountCalculator:
    def __init__(self, config: DiscountConfig): ...
    def apply_discount(self, amount: float) -> float: ...
```

**Good (3 lines):**

```python
def calculate_discount(amount: float, percent: float) -> float:
    return amount * (percent / 100)
```

## Consolidate duplicated logic

Two near-identical try/except blocks, or the same three lines repeated across branches,
collapse to one. Pull the shared part out; keep only what genuinely differs.

**Bad:**

```python
try:
    data = load(path)
except FileNotFoundError:
    logger.error("missing %s", path); raise
except PermissionError:
    logger.error("missing %s", path); raise
```

**Good:**

```python
try:
    data = load(path)
except (FileNotFoundError, PermissionError):
    logger.error("cannot read %s", path)
    raise
```

## Collapse conditional chains with early returns

Deep nesting (>4 levels) hides the happy path. Return early on the guard cases; the main
logic drops a level each time. A whole method → `extract-method.md`.

**Bad:**

```python
def process(user):
    if user is not None:
        if user.active:
            if user.has_quota():
                return do_work(user)
            else:
                return None
        else:
            return None
    else:
        return None
```

**Good:**

```python
def process(user):
    if user is None or not user.active or not user.has_quota():
        return None
    return do_work(user)
```

## Delete speculative flexibility

`review complexity` → `import-count` (often drags in a config lib).

Configuration, hooks, and "modes" that nothing requests are cost without value. Remove the
unused knob; the call site gets shorter and the reader stops wondering when the branch fires.

**Bad:** a `save_preferences(prefs, *, cache=False, merge_mode="replace", notify=None)` where
every caller passes the defaults.

**Good:** `save_preferences(prefs)` — add a parameter the day a caller needs it.

## Drop handling for impossible states

`review complexity` → `cyclomatic-complexity`. Name why the state can't occur before cutting (`../SKILL.md`).

```python
# BEFORE — enum has two members; the else is dead
if status == Status.OK:
    ...
elif status == Status.FAIL:
    ...
else:
    raise RuntimeError("unreachable")  # speculative

# AFTER — exhaustive match, no dead branch
match status:
    case Status.OK: ...
    case Status.FAIL: ...
```

## Tighten over-long docstrings

A docstring longer than its function, restating the signature, is prose over-engineering.
Cut to a one-line summary. Full rule + examples: `docstring-style.md`.
