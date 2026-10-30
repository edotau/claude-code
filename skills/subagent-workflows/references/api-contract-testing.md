# API & Contract Testing — DRF/FastAPI endpoints and data jobs

Load when testing a Django/DRF endpoint, a FastAPI route or another service boundary, or when "unit tests pass but
integration breaks". Boundary rules (error paths first, shape not status, sensitive fields absent):
`rules/standards/testing-guidelines.md`. Run in-process (DRF `APIClient` + `force_authenticate`, transactional
rollback); reserve `httpx` for cross-service or deployed checks.

## Find the endpoints first

Coverage follows what exists, not what you remember:

```bash
grep -rn "router.register\|DefaultRouter\|SimpleRouter" --include="*.py" .        # DRF routers
grep -rn "path(\|re_path(" --include="*.py" . | grep -i url                       # explicit url patterns
grep -rln "serializers.ModelSerializer\|serializers.Serializer" --include="*.py" .  # the contract
grep -rn "@\(app\|router\)\.\(get\|post\|put\|patch\|delete\)" --include="*.py" .  # FastAPI
```

Per route, read the view + serializer for required fields, permission classes (`IsAuthenticated`,
`DjangoModelPermissions`, object-level checks) and declared status codes. The serializer is the contract: assert
against it, not a hand-typed dict.

## The two matrices (per endpoint)

| Auth / permission case | Expected |
| --- | --- |
| No `Authorization` header | 401 |
| Malformed, garbage or expired token | 401 |
| Valid token, insufficient role/permission | 403 |
| Valid token, not the object owner | 403 (or 404 when existence is hidden) |
| Valid token, correct permission | 2xx |

Test the missing header apart from the invalid token: DRF routes them through authentication vs permission and they
regress independently.

| Input case (POST/PUT/PATCH) | Expected |
| --- | --- |
| Empty body `{}`; each required field missing, one at a time; `null` for a required field | 400 |
| Wrong type (str where int expected) | 400 |
| Boundary: min−1 / min / max / max+1 | 400 / 2xx / 2xx / 400 |
| Injection or oversized free text | 400, or 2xx sanitized — never 500 |
| Unknown/extra field | the serializer's policy — assert the one chosen |

## DRF pattern

```python
@pytest.fixture
def auth_client(db, django_user_model):
    user = django_user_model.objects.create_user("alice", password="x")
    c = APIClient()
    c.force_authenticate(user=user)        # tests the view, not the auth backend
    return c, user

@pytest.mark.django_db
def test_create_rejects_missing_name(auth_client):
    c, _ = auth_client
    resp = c.post("/api/widgets/", {}, format="json")
    assert resp.status_code == 400 and "name" in resp.json()

@pytest.mark.django_db
def test_create_returns_contract_shape(auth_client):
    c, _ = auth_client
    body = c.post("/api/widgets/", {"name": "w"}, format="json").json()
    assert {"id", "name", "created_at"} <= body.keys() and "owner_secret" not in body
```

## Suite hygiene

- One module (or class) per endpoint; name by behavior (`test_returns_403_when_not_owner`).
- Factories/fixtures, never hardcoded IDs — they differ per environment.
- Each test independent: `django_db` rolls back; clean external state explicitly.
- Rate-limit/throttle tests run last and serially — they interfere with parallel suites.

## Data-job boundaries

For a batch job or warehouse query the contract is the input and output schema: assert output columns and dtypes,
test empty and malformed input, stub the warehouse client in unit tests, and keep live calls in a marked
`@pytest.mark.integration` suite.
