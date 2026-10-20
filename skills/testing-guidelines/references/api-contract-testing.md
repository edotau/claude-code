# API & Contract Testing — DRF endpoints and service contracts

**Load this reference when:** generating tests for a Django/DRF endpoint, a FastAPI route,
or any HTTP/service boundary — or when "the unit tests pass but integration breaks."

Unit tests prove a function behaves. Contract tests prove a *boundary* behaves: the request
shape it accepts, the status codes it returns, the response shape callers depend on, and the
auth it enforces. 80% of bugs live in the error paths — test those first, not the happy path.

## Find the endpoints first

Enumerate routes before writing a single test, so coverage is driven by what exists, not by
what you remember:

```bash
# DRF: ViewSet router registrations + explicit url patterns
grep -rn "router.register\|DefaultRouter\|SimpleRouter" --include="*.py" .
grep -rn "path(\|re_path(" --include="*.py" . | grep -i url

# DRF: serializers define the request/response contract — read these for field shape
grep -rln "serializers.ModelSerializer\|serializers.Serializer" --include="*.py" .

# FastAPI (if present)
grep -rn "@\(app\|router\)\.\(get\|post\|put\|patch\|delete\)" --include="*.py" .
```

For each route, read the view + serializer to learn: required fields, auth/permission
classes (`IsAuthenticated`, `DjangoModelPermissions`, object-level checks), and the declared
status codes. The serializer is the contract — assert against it, not against a hand-typed dict.

## The two matrices

Generate these per endpoint. They are checklists, not happy-path smoke tests.

### Auth / permission matrix (every protected endpoint)

| Case | Expected |
|------|----------|
| No `Authorization` header | 401 |
| Malformed / garbage token | 401 |
| Expired token | 401 |
| Valid token, insufficient role/permission | 403 |
| Valid token, not the object owner | 403 (or 404 if existence is hidden) |
| Valid token, correct permission | 2xx |

Test the *missing header* case separately from the *invalid token* case — DRF routes them
through different code (authentication vs. permission) and they regress independently.

### Input-validation matrix (every POST/PUT/PATCH with a body)

| Case | Expected |
|------|----------|
| Empty body `{}` | 400 |
| Each required field missing (one at a time) | 400 |
| Wrong type (str where int expected) | 400 |
| Boundary: min−1 / min / max / max+1 | 400 / 2xx / 2xx / 400 |
| `null` for a required field | 400 |
| Injection/oversized string in a free-text field | 400 or 2xx-sanitized (never 500) |
| Unknown/extra field | per serializer policy — assert the one you chose |

A 500 on bad input is always a finding: validation should reject with 400, not crash.

## DRF pattern (pytest + APIClient)

```python
import pytest
from rest_framework.test import APIClient

@pytest.fixture
def client():
    return APIClient()

@pytest.fixture
def auth_client(db, django_user_model):
    user = django_user_model.objects.create_user("alice", password="x")
    c = APIClient()
    c.force_authenticate(user=user)        # bypasses token plumbing; tests the view, not auth backend
    return c, user

@pytest.mark.django_db
def test_create_requires_auth(client):
    resp = client.post("/api/widgets/", {"name": "w"}, format="json")
    assert resp.status_code == 401

@pytest.mark.django_db
def test_create_rejects_missing_name(auth_client):
    c, _ = auth_client
    resp = c.post("/api/widgets/", {}, format="json")
    assert resp.status_code == 400
    assert "name" in resp.json()            # assert the field, not just the code

@pytest.mark.django_db
def test_create_returns_contract_shape(auth_client):
    c, _ = auth_client
    resp = c.post("/api/widgets/", {"name": "w"}, format="json")
    assert resp.status_code == 201
    body = resp.json()
    assert {"id", "name", "created_at"} <= body.keys()
    assert "owner_secret" not in body      # sensitive fields must never leak
```

## Plain-HTTP services (pytest + httpx)

For non-Django services or when hitting a running endpoint:

```python
import httpx

def test_health(base_url):
    r = httpx.get(f"{base_url}/health", timeout=5)
    assert r.status_code == 200
    assert r.json()["status"] == "ok"
```

Prefer DRF's `APIClient` for in-process Django tests (no network, transactional DB rollback
per test). Reserve `httpx` for cross-service or deployed-endpoint checks.

## Rules that keep contract suites honest

1. **One test module (or class) per endpoint** — failures stay isolated and readable.
2. **Factories/fixtures, never hardcoded IDs** — `factory_boy` or fixtures; IDs differ per env.
3. **Assert the response *shape*, not just the status** — a 200 with the wrong body is still a bug.
4. **Assert sensitive fields are absent** — passwords, tokens, internal IDs never in responses.
5. **Each test independent** — `@pytest.mark.django_db` rolls back; for external state, clean up explicitly.
6. **Rate-limit / throttle tests run last and serially** — they interfere with parallel suites.
7. **Name by behavior** — `test_returns_403_when_not_owner`, not `test_perms_3`.

## Data-job boundaries

The same discipline applies to non-HTTP boundaries. For a batch job or warehouse query,
the "contract" is the input table schema and the output schema:

- Assert the output DataFrame's columns and dtypes (the schema is the contract).
- Test the empty-input and malformed-row cases, not just a populated happy-path table.
- Mock the warehouse/compute client at the boundary; don't hit a live warehouse in a
  unit test. Reserve live calls for a marked `@pytest.mark.integration` suite.
