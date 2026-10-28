# Docstring brevity — 1 line, ≤120 characters

A docstring earns its place by saying what the signature can't. When it restates the
parameter names, narrates the obvious, or pads with ceremony, it's over-engineering in
prose — the same instinct that produces 150-line abstractions for 3-line problems.

## The rule

Canonical rule: `rules/standards/communication.md` (Comment Brevity — 1 line max, ≤120 chars,
applies to docstrings too). This file is the how-to:

- Lead with a one-line imperative summary ("Return the discount-adjusted amount.").
- Drop anything the reader gets for free from the signature and type hints.
- When the *why* is non-obvious, say it in that one line rather than the *what* (see "When to relax").

## When to relax

This is judgment, not a checker — there is deliberately no linter script. Even a complex public API
(a library entry point, subtle invariants or units) gets one line (≤120 chars); detail belongs in the repo's docs.
The rule targets the common case: internal functions whose six-line docstring says less than their name already does.

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
