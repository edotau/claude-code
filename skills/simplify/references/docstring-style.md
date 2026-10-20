# Docstring brevity — ≤120 characters or ≤2 lines

A docstring earns its place by saying what the signature can't. When it restates the
parameter names, narrates the obvious, or pads with ceremony, it's over-engineering in
prose — the same instinct that produces 150-line abstractions for 3-line problems.

## The rule

- **≤120 characters, or ≤2 lines** — whichever the surrounding code favors.
- Lead with a one-line imperative summary ("Return the discount-adjusted amount.").
- Drop anything the reader gets for free from the signature and type hints.
- When the *why* is non-obvious, a short second line for it beats a long first line.

## When to relax

This is judgment, not a checker — there is deliberately no linter script. A genuinely
complex public API (a library entry point, a function with subtle invariants or units)
may need a fuller docstring with Args/Returns/Raises. The rule targets the common case:
internal functions whose six-line docstring says less than their name already does.

## Before / after

**Bad — restates the signature, narrates the obvious:**
```python
def calculate_discount(amount: float, percent: float) -> float:
    """
    Calculate the discount.

    This function takes an amount and a percentage and computes the discount
    by multiplying the amount by the percentage divided by one hundred. It
    then returns the resulting discounted value as a float.

    Args:
        amount: The amount (a float).
        percent: The percent (a float).
    Returns:
        The discount as a float.
    """
    return amount * (percent / 100)
```

**Good — one line, says what the signature can't:**
```python
def calculate_discount(amount: float, percent: float) -> float:
    """Return the discount value for `amount` at `percent` (0–100)."""
    return amount * (percent / 100)
```

**Bad — ceremony around a private helper:**
```python
def _slugify(name: str) -> str:
    """
    Slugify the given name.

    Converts the provided name string into a URL-safe slug suitable for use
    in paths and identifiers.
    """
    ...
```

**Good — drop it or make it one line:**
```python
def _slugify(name: str) -> str:
    """Lowercase, hyphenate, strip non-alphanumerics for a URL-safe slug."""
    ...
```

## The test

If the docstring is longer than the function and tells the reader nothing the code doesn't,
cut it to one line — or delete it and let a clear name carry the meaning.
