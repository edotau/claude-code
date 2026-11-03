# Test-and-Fix Loops

How to run the loop behind `rules/standards/testing-guidelines.md`, alone or as subagent loops.

## The loop (one leaf, one scope)

1. Run the scoped set: `go test ./internal/<pkg> -run '<Test>' -count=1` (`-race` for concurrent code); other
   languages: `rules/software/<lang>/testing.md`. The full suite is the main thread's, once, with approval.
2. Triage each failure: test-wrong vs code-wrong, with evidence.
3. Code wrong: reproduce live, state "X because Y", fix, re-run the same command, add the regression test, prove its
   teeth.
4. At most 3 cycles per test, 5 for the whole task; then report: fixed (which kind), still failing with output, tests
   planned but not written.

## Loop shapes

| Situation | Shape |
| --- | --- |
| One failing test or package | the loop yourself, or one `copilot` leaf |
| Failures across packages | one `copilot` leaf per package in ONE message, each owning only its `_test.go` files |
| A change being implemented | `code-worker` ‖ `copilot` in one message; copilot repairs now, writes new tests after "landed" |
| A fixed bug | after your live before/after, one leaf writes the regression test + `test mutate` proof |
| Suite must go green, bounded | `Workflow green-keeper-loop` — only when the user asks |

A leaf at its cap reports and stops; it never widens scope.

## Prove the teeth

```bash
claude-code test mutate <file> <old> <new> [--run RE] <pkg>   # exit 0 teeth, 1 vacuous, 2 error
```

It mutates a `go test -overlay` copy, never the tree. Vacuous means the test passes with the defect: often another
path produces the same output (assert on something only the fixed path emits).
