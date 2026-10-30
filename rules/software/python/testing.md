---
paths:
  - "**/*.py"
  - "**/*.pyi"
---
# Python Testing

> This file extends [standards/testing-guidelines.md](../../standards/testing-guidelines.md)
> with Python specific content.

## Run it scoped

| Need | Command |
| --- | --- |
| One file / one test | `.venv/bin/python -m pytest <path> -x -v` · `... -k '<name>'` |
| A marked tier | `pytest -m unit` (`@pytest.mark.unit` / `integration` / `slow` in the repo's `pytest.ini` markers) |
| Repo tiers | `make test-smoke`/`test-unit`/`test-all` if defined, else `make test-unit`, else `make test` |

The import rule, in Python: `grep -nE '^(import|from)' <test> | grep <module>` — no hit, no guard.

## pytest quick reference

| Need | Use |
| --- | --- |
| Setup/teardown, shared data | `@pytest.fixture` (function/class/module/package/session), `conftest.py` |
| Input matrix | `@pytest.mark.parametrize("a,b", [...])` |
| Isolate an external call | `@patch("consumer.module.func", autospec=True)` — patch where it's used, not where it's defined |
| Expect an exception | `with pytest.raises(ValueError, match="...")` |
| Optional tests | `@pytest.mark.skipif(...)`; async: `@pytest.mark.asyncio` |

## A unit test must not leave the box

A cloud client built with its default credential chain reaches the network (boto3 with `session=None` probed
`169.254.169.254`, 2.15s per test). Pass a stub session (`session=MagicMock()`) or inject the client.

Audit only when asked: a conftest plugin wrapping `socket.socket.connect`/`connect_ex`/`socket.getaddrinfo` (loopback
allowed) that RECORDS each connection per `item.nodeid` names every offender in one run without failing it. The same
shape on `unittest.mock._patch.__enter__/__exit__` finds vacuous mocks: `call_count == 0` AND `mock_calls == []`.
Neither sees a subprocess's traffic.

## Endpoints and contracts

DRF/FastAPI contract suites (`APIClient`/`httpx` patterns; one case per risk found, no matrices):
`skills/subagent-workflows/references/api-contract-testing.md`.
