# Regression guard examples

## The import rule

It is easy to write a test that constructs the same data structures the function uses,
performs the same arithmetic inline, and asserts the expected result. It passes. It explains
the bug clearly. And it would pass just as happily if someone reverted the fix, because it
never touches the fixed code.

Check for this directly:

```bash
grep -nE '^(import|from)' <test-file> | grep <module-under-test>
```

No hit means no guard, whatever the test is named. Keep such tests for their explanatory
value if you like — but do not count them as protection, and do not describe the fix as
"verified" on their strength.

## Prove the teeth empirically

Step 4 is not ceremony. Reverting a fix inside a real repository to test something and then
leaving it partly reverted is a genuinely bad outcome; the checksum is what lets you state
that the tree is unchanged rather than hope so.

## Reproduce the whole fault, not part of it

Concretely: a defect zeroed the delta for a candidate when peptide predictions were missing.
The first test starved one window out of fifteen — the total stayed non-zero, so the
assertion held even with the fix reverted. Starving *every* affected window reproduced the
real signature (`0.0 == 0.0 : starved candidate reported as neutral`) and the test began to
bite.

## Anti-vacuity for equivalence tests

When asserting that an optimised path agrees with a reference path, the optimised path often
has a **fallback**. If it silently falls back, both paths become the same code and the
equivalence assertion is trivially true — the test passes while testing nothing.

```python
# not just: assert fast_result.equals(reference_result)
# also:     assert requested == "unique_peptides.fst"   # i.e. it did NOT fall back
```

## Derive assertions from the fixture

Hardcoding a value you believe is in the fixture (`assert name == "cand_S20W"`) breaks when
position 20 turns out to hold a different residue, and the failure looks like a code bug
rather than a test bug. Compute it: `f"cand_{PARENT[19]}20W"`.

## Failing loudly beats degrading quietly

The defect above existed because a missing prediction was logged at
DEBUG and execution carried on, producing a plausible-looking "no impact" answer. Raising
into an existing fallback handler turned a silent wrong result into a slower correct one.

## Pin the behavior before the assertion

```
Given an authenticated user with no widgets
When they POST /api/widgets/ with {"name": "w"}
Then the response is 201 and the body contains the new widget's id
```

Read these examples alongside the discipline rules in [the skill](../SKILL.md).
