---
description: "Run tests and fix any failures"
argument-hint: "[fast | unit | all | path/to/test.py::test_name]"
---

1. If `$ARGUMENTS` is a specific test path, run it directly: `.venv/bin/python -m pytest $ARGUMENTS -x -v`
   for Python, `go test ./<pkg> -run '<Name>'` for Go
2. If `$ARGUMENTS` is "fast", "unit", or "all", run the matching `make test-smoke` / `test-unit` /
   `test-all` target when the Makefile defines it (`make -n <target> >/dev/null 2>&1`); otherwise
   fall back to `make test`
3. Otherwise, run `make test-unit` if defined, else `make test`
4. (steps below assume the run in 2 or 3)
5. If all tests pass, report success
6. If tests fail:
   - Read the failing test file and the source file it tests
   - Identify root cause: is the test wrong or the implementation?
   - Fix the issue (minimal change)
   - Re-run only that test (`.venv/bin/python -m pytest tests/path/test_file.py::test_name -x -v`,
     or `go test ./<pkg> -run '<Name>'`)
   - Repeat until it passes, then re-run the full suite
   - Max 3 fix attempts per test — if stuck, report and move on
7. After all tests pass, run `make fmt` to auto-format any changes

Be methodical: fix one test at a time. In Python repos never use bare `python` or `pytest` — always the `.venv/bin/` prefix.
