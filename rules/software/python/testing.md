---
paths:
  - "**/*.py"
  - "**/*.pyi"
---
# Python Testing

> This file extends [standards/testing-guidelines.md](../../standards/testing-guidelines.md)
> with Python specific content.

## Framework

Use **pytest** as the testing framework.

## Coverage

```bash
pytest --cov=src --cov-report=term-missing
```

## Test Organization

Use `pytest.mark` for test categorization:

```python
import pytest

@pytest.mark.unit
def test_calculate_total():
    ...

@pytest.mark.integration
def test_database_connection():
    ...
```

## A Unit Test Must Not Leave the Box

A cloud client built with its default credential chain reaches the network (boto3 with `session=None` probed
`169.254.169.254`, 2.15s per test). Pass a stub session (`session=MagicMock()`) or inject the client.

Audit, don't assume: a conftest plugin wrapping `socket.socket.connect`/`connect_ex`/`socket.getaddrinfo` (loopback
allowed) that RECORDS each connection per `item.nodeid` names every offender in one run without failing it. The same
shape on `unittest.mock._patch.__enter__/__exit__` finds vacuous mocks: `call_count == 0` AND `mock_calls == []`.
Neither sees a subprocess's traffic.

## Reference

Rules: `rules/standards/testing-guidelines.md`; pytest quick reference and the optional test-and-fix loop: the
`testing-guidelines` skill.
