---
paths:
  - "**/*.py"
  - "**/*.pyi"
---
# Python Testing

Extends [testing-guidelines.md](../../standards/testing-guidelines.md).

- Run scoped: `.venv/bin/python -m pytest <path> -x -v`, `-k '<name>'`, or a tier from `pytest.ini` (`-m unit`).
  Use the repo's test targets; full suites follow `quality.md` → Verification Order.
- Confirm the test imports its subject; patch where used: `@patch("consumer.module.func", autospec=True)`.
- Stub cloud sessions (`session=MagicMock()`) or inject clients; default credential chains can reach the network.
- Only when asked, audit network calls with a conftest plugin recording `socket.connect`/`getaddrinfo`
  per `item.nodeid` (loopback allowed); it cannot see subprocess traffic.

## Endpoints and contracts

- Find routes in code (`router.register`, `path(`, FastAPI decorators); assert the serializer's contract.
- One spanning test per endpoint, covering only its branches: 401 missing/bad token, 403/404 role/non-owner,
  400 missing/wrong-type/null required fields, never 500 on free text; required keys present, sensitive keys absent.
- DRF: in-process `APIClient`, `force_authenticate`, `@pytest.mark.django_db`, factories, no fixed IDs.
  Use `httpx` only for cross-service/deployed checks; throttle tests last and serial.
- Data jobs: assert columns, dtypes, empty/malformed input; stub the warehouse, mark live calls `integration`.
