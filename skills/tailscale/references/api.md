# Tailscale API

The REST API surface is large and changes over time: treat the examples below as orientation and **WebFetch
<https://tailscale.com/docs/reference/tailscale-api>** for exact request/response shapes, query parameters and fields.

## Mental model

- **Base URL**: `https://api.tailscale.com/api/v2`.
- **Tailnet path**: tailnet-scoped endpoints take `{tailnet}`; `-` means the caller's default (`/tailnet/-/devices`).
- **Auth**:
  - **API access tokens** (`tskey-api-...`) — user-scoped, generated in the admin console, expire in 1–90 days.
  - **OAuth client credentials** (`tskey-client-...`) — machine-scoped, scope-restricted (`devices:read`,
    `devices:write`, `dns:read`, others), exchanged for short-lived access tokens.
  - **Trust credentials** — delegated fine-grained access with attribute-based limits, for API access tied to
    specific key/value claims rather than full admin rights.
- **Body format**: JSON for most endpoints; HuJSON (comments + trailing commas) for the ACL endpoint.
- **No pagination** today — list endpoints return everything in one response.
- **No published rate limits** — but expect throttling on hot loops; back off on 429s.

## Canonical shapes

### Authenticate

```bash
# Basic auth (token as username, empty password) — works with any token
curl -u "$TOKEN:" https://api.tailscale.com/api/v2/tailnet/-/devices

# Bearer
curl -H "Authorization: Bearer $TOKEN" https://api.tailscale.com/api/v2/tailnet/-/devices

# OAuth client credentials → access token
curl -X POST https://api.tailscale.com/api/v2/oauth/token \
  -d "client_id=$CLIENT_ID" \
  -d "client_secret=$CLIENT_SECRET" \
  -d "grant_type=client_credentials"
```

### Device endpoints

| Call | Purpose |
|---|---|
| `GET /tailnet/-/devices` | List devices; fields include `id`, `hostname`, `addresses`, `tags`, `os`, `lastSeen` (ISO timestamp) |
| `GET` / `DELETE /device/{id}` | Get or delete one device |
| `POST /device/{id}/authorized` `{"authorized": true}` | Authorize / deauthorize (auto-approval webhook pattern) |
| `POST /device/{id}/expire` | Force re-auth (expire the device key) |
| `POST /device/{id}/tags` `{"tags": [...]}` | Replace device tags |

### Create an auth key

```bash
curl -X POST -u "$TOKEN:" -H "Content-Type: application/json" \
  -d '{
    "capabilities": {
      "devices": {
        "create": {
          "reusable": true,
          "ephemeral": true,
          "preauthorized": true,
          "tags": ["tag:ci"]
        }
      }
    },
    "expirySeconds": 86400,
    "description": "CI runner key"
  }' \
  https://api.tailscale.com/api/v2/tailnet/-/keys
```

The key value is returned **only once** in the response.

### Update the policy file (ACL)

```bash
# GET returns HuJSON + an ETag for optimistic concurrency control
curl -u "$TOKEN:" https://api.tailscale.com/api/v2/tailnet/-/acl

# POST with If-Match: <etag> to prevent races
curl -X POST -u "$TOKEN:" -H "Content-Type: application/json" \
  -H "If-Match: \"$ETAG\"" \
  -d @policy.json \
  https://api.tailscale.com/api/v2/tailnet/-/acl

# Preview / validate before applying
curl -X POST -u "$TOKEN:" -H "Content-Type: application/json" \
  -d '{"src":"alice@example.com","dst":"tag:server"}' \
  https://api.tailscale.com/api/v2/tailnet/-/acl/preview
```

## Automation patterns

- **Atomic policy updates**: always pass `If-Match` with the GET-returned `ETag`; without it a concurrent edit is
  silently overwritten.
- **Bulk delete stale devices**: list devices, filter by `lastSeen`, `DELETE /device/{id}` each; throttle for 429s.
- **Device ID**: `tailscale status --json` locally beats an API round-trip.
- **Programmatic device approval**: webhook on `nodeNeedsApproval` → check external state →
  `POST /device/{id}/authorized`.

## Where to find current information

Always fetch rather than reconstruct: endpoint specifics (reference page), OAuth scope names (they evolve), webhook
payloads and signature verification, logging fields and event types.

| User is asking about… | Fetch |
|---|---|
| Full REST API reference (endpoints, fields, query parameters) | <https://tailscale.com/docs/reference/tailscale-api> |
| OAuth clients — scopes, secret handling, ephemeral flag | <https://tailscale.com/docs/features/oauth-clients> |
| Trust credentials (delegated/fine-grained API access) | <https://tailscale.com/docs/reference/trust-credentials> |
| Webhooks — overview, events, payload shapes | <https://tailscale.com/docs/features/webhooks> |
| Rotate a webhook signing secret | <https://tailscale.com/docs/features/webhooks/how-to/rotate-webhook-secret> |
| Logging overview | <https://tailscale.com/docs/features/logging> |
| Configuration audit logs | <https://tailscale.com/docs/features/logging/audit-logging> |
| Network flow logs | <https://tailscale.com/docs/features/logging/network-flow-logs> |
| Log streaming (SIEM, S3) | <https://tailscale.com/docs/features/logging/log-streaming> |
| Logging streaming event schema | <https://tailscale.com/docs/reference/logging-streaming-events> |
| `tsnet` (embed a Tailscale node in a Go program) | <https://tailscale.com/docs/reference/tsnet-server-api> |
