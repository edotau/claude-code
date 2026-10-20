# pytest Patterns — Fixtures, Parametrize, Mocking, Markers

**Load this reference when:** writing or reviewing pytest fixtures, parametrized tests,
mocks/patches, markers, exception/async tests, or organizing a `tests/` tree.

## Assertions

```python
assert result == expected                    # Equality
assert item in collection                    # Membership
assert result is None                        # Identity
assert isinstance(obj, MyClass)              # Type checking
assert len(items) == 3                       # Length
assert result == pytest.approx(3.14, rel=1e-2)  # Float comparison
```

## Fixtures

```python
import pytest

@pytest.fixture
def sample_data():
    """Provide test data — runs before each test that requests it."""
    return {"key": "value", "count": 42}

@pytest.fixture(scope="module")
def db_connection():
    """Expensive setup — shared across module, torn down after."""
    conn = create_connection()
    yield conn
    conn.close()

@pytest.fixture(params=["small", "medium", "large"])
def dataset_size(request):
    """Parametrized fixture — test runs once per param."""
    return request.param
```

**Scopes**: `function` (default), `class`, `module`, `package`, `session`

**conftest.py**: Place shared fixtures in `conftest.py` at the appropriate directory level. pytest discovers them automatically.

## Parametrize

```python
@pytest.mark.parametrize("input_val,expected", [
    ("hello", 5),
    ("", 0),
    ("world", 5),
])
def test_string_length(input_val, expected):
    assert len(input_val) == expected

# Multiple parametrize decorators = cartesian product
@pytest.mark.parametrize("x", [1, 2])
@pytest.mark.parametrize("y", [10, 20])
def test_multiply(x, y):
    assert x * y > 0
```

## Mocking and Patching

```python
from unittest.mock import patch, MagicMock, PropertyMock

# Patch at the consumer module, not the definition
@patch("myapp.service.requests.get")
def test_api_call(mock_get):
    mock_get.return_value.json.return_value = {"status": "ok"}
    result = my_service_function()
    assert result == "ok"
    mock_get.assert_called_once()

# Context manager style
def test_with_context():
    with patch("myapp.module.external_func") as mock_func:
        mock_func.return_value = 42
        assert my_function() == 42

# Patch exceptions
@patch("myapp.client.api_call", side_effect=ConnectionError("timeout"))
def test_handles_error(mock_call):
    result = resilient_function()
    assert result is None

# autospec for type-safe mocks
@patch("myapp.service.Client", autospec=True)
def test_with_autospec(MockClient):
    instance = MockClient.return_value
    instance.fetch.return_value = "data"
    # Catches method signature mismatches
```

## Markers

```python
@pytest.mark.slow           # Excluded by default in addopts
@pytest.mark.benchmark      # Excluded by default
@pytest.mark.skipif(shutil.which("docker") is None, reason="docker not installed")
@pytest.mark.parametrize(...)
```

## Testing Exceptions

```python
def test_raises_on_invalid_input():
    with pytest.raises(ValueError, match="must be positive"):
        process_data(-1)
```

## Async Testing

```python
import pytest

@pytest.mark.asyncio
async def test_async_operation():
    result = await async_function()
    assert result == expected
```

## Test Organization

```
tests/
├── conftest.py          # Shared fixtures
├── unit/
│   ├── test_models.py
│   └── test_utils.py
├── integration/
│   └── test_api.py
└── fixtures/
    └── sample_data.json
```

## Anti-Patterns to Avoid

- **Testing implementation, not behavior**: Test what it does, not how it does it
- **Excessive mocking**: If you mock everything, you test nothing
- **Shared mutable state**: Each test must be independent
- **Test interdependence**: Tests must pass in any order
- **Ignoring test failures**: A failing test is a bug — fix it or delete it
- **Testing framework internals**: Trust your framework's guarantees
