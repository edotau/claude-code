---
paths:
  - "**/*.py"
  - "**/*.pyi"
---
# Python Testing

> This file extends [standards/quality.md](../../standards/quality.md) with Python specific content.

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

Constructing a cloud client with its DEFAULT credential chain reaches the network. Measured: a boto3 client
built with `session=None` probed the instance-metadata service at `169.254.169.254` and cost **2.15s** on a
non-EC2 box — enough to be the second-slowest test in a 7,400-test suite. Pass an explicit stub session
(`session=MagicMock()`), or inject the client.

Audit it rather than assuming: a ~60-line plugin that wraps `socket.socket.connect`/`connect_ex`/
`socket.getaddrinfo`, allows loopback, attributes each connection to `item.nodeid` in
`pytest_runtest_setup`, and RECORDS instead of blocking will name every offender in one run without
turning the suite red. The same shape audits vacuous mocks: wrap `unittest.mock._patch.__enter__/__exit__`
and report any mock ending with `call_count == 0` AND `len(mock_calls) == 0` (check both — `mock_calls`
catches attribute chains like `client.files.get_metadata(...)`). Neither can see a SUBPROCESS's traffic.

## Reference

See skill: `testing` for detailed pytest patterns and fixtures.
