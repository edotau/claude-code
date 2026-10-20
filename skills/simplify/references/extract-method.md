# Extract-Method — reduce a method's cognitive complexity

The focused playbook for one high-value simplification (#2): a single method has grown a
high **cognitive complexity** (nested conditionals, if-else/switch chains, deep loops,
tangled boolean expressions) and reads as one long tangle. Cut it down by extracting logic
into focused helpers so the main method reads as a high-level flow.

This is the method-level form of "collapse conditional chains and deep nesting" from the
skill's Process step 3. The contract is unchanged: **behavior is preserved** — you change
*how* the code is structured, never *what* it does. Surface the assumption before you cut
(see SKILL.md "Surface the assumption before you cut") — extraction that silently drops a
guard or "impossible" branch is a behavior change disguised as a cleanup.

Pick a concrete target. Most teams gate on a number — e.g. SonarQube **cognitive
complexity ≤ 15**. Use whatever threshold the project enforces; if none, "a senior engineer
wouldn't call it overcomplicated" is the bar.

## Procedure

1. **Analyze the current method** — locate the complexity sources:
   - Nested conditionals; long if-else / `match` / `switch` chains
   - Repeated blocks that differ only by a value or type
   - Multiple loops carrying their own conditions
   - Complex boolean expressions inline in a branch

2. **Identify extraction opportunities:**
   - Validation / guard logic → a dedicated predicate or validator
   - Type- or case-specific processing that repeats → one handler per case
   - A self-contained transformation or calculation → a named helper
   - A pattern that appears more than once → one helper, called twice

3. **Extract focused helpers** (do this *before* rewriting the main flow):
   - One clear responsibility per helper; name it for that responsibility.
   - Keep helpers private / module-local and close to their caller.
   - Guard clauses and null/empty checks go early, so the happy path stays flat.
   - When a helper must return several values, return a small struct — a `dataclass`
     (Python) or a typed object/tuple (TS) — rather than out-params.

4. **Simplify the main method:**
   - Reduce nesting depth; prefer early returns over `else` ladders.
   - Replace a large if-else chain with a small dispatch (`match`/`switch`, or a
     lookup table mapping case → handler) where it reads cleaner.
   - The body should now read as an orchestration of named steps.

5. **Preserve functionality:**
   - Same inputs → same outputs, same side effects.
   - Keep every validation and error path; preserve exception types and messages.
   - Pass all parameters through to the helpers intact.

## Verify — don't assume

Behavior preservation is only proven by a green run, not by reading the diff. Run the same
tests and lint that passed before the extraction (`make test && make lint`, or the
change's targeted tests) and **read the actual output** — confirm zero failures rather than
assuming they passed. If a test fails, the extraction changed behavior: find the dropped
guard / empty-collection check / inverted condition, restore it, re-run. If no test covers
the method you're restructuring, that absence *is* a finding — say so before cutting, since
you have no safety net. Finally, confirm the complexity metric is now at or below target.
