---
paths:
  - "**/*.py"
  - "**/*.pyi"
---
# Python Testing

> This file extends [standards/testing-guidelines.md](../../standards/testing-guidelines.md)
> with Python specific content.

- Run scoped: `.venv/bin/python -m pytest <path> -x -v` (`-k '<name>'`), or a marked tier (`-m unit`; markers live
  in the repo's `pytest.ini`); repo tiers via `rtk make test-smoke|test-unit|test-all`, else the repo's test target
  (e.g. `rtk make test`, `pytest`).
- Import rule: `rtk grep -nE '^(import|from)' <test> | grep <module>`; no hit, no guard.
- Patch where a name is used, not defined: `@patch("consumer.module.func", autospec=True)`.
- A unit test must not leave the box: a cloud client on its default credential chain can reach the network, so pass
  a stub session (`session=MagicMock()`) or inject the client.
- Network audit, only when asked: a conftest plugin that records `socket.connect`/`getaddrinfo` per `item.nodeid`
  (loopback allowed) lists every offender in one run; it cannot see a subprocess's traffic.

## Endpoints and contracts

- Find routes from code (`router.register`, `path(`, FastAPI decorators); the serializer is the contract, so assert
  against it, not a hand-typed dict.
- One spanning test per endpoint, only the branches the view has: 401 missing vs bad token, 403/404 role or
  non-owner, 400 missing/wrong-type/null required field, never a 500 on free text; response keys present and
  sensitive fields absent.
- DRF in-process: `APIClient` + `force_authenticate`, `@pytest.mark.django_db`, factories (no hardcoded IDs);
  `httpx` only for cross-service or deployed checks; throttle tests last and serial.
- Data jobs: assert output columns and dtypes plus empty and malformed input; stub the warehouse client and mark
  live calls `@pytest.mark.integration`.
