# Simplification patterns

The recurring wins when reducing complexity. Each is behavior-preserving — you change how
the code reads, not what it does. For the fuller over-abstraction gallery (TypeScript,
shell, speculative features), see `skills/evaluate-code/references/anti-patterns.md`.

## Inline single-use abstractions

A class that wraps one method, or a helper called from one place, is indirection without
payoff. Inline it; add the abstraction back when a second caller actually appears.

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
logic drops a level each time.

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

Configuration, hooks, and "modes" that nothing requests are cost without value. Remove the
unused knob; the call site gets shorter and the reader stops wondering when the branch fires.

**Bad:** a `save_preferences(prefs, *, cache=False, merge_mode="replace", notify=None)` where
every caller passes the defaults.

**Good:** `save_preferences(prefs)` — add a parameter the day a caller needs it.

## Tighten over-long docstrings

A docstring longer than its function, restating the signature, is prose over-engineering.
Cut to a one-line summary. Full rule + examples: `docstring-style.md`.
