# Tailscale API

The REST surface is large and changes: treat examples as orientation and **WebFetch
<https://tailscale.com/docs/reference/tailscale-api>** for exact shapes, parameters and fields.

## Mental model

- **Base URL**: `https://api.tailscale.com/api/v2`; tailnet endpoints take `{tailnet}`, `-` = caller's default
  (`/tailnet/-/devices`).
- **Auth**:
  - **API access tokens** (`tskey-api-...`) — user-scoped, admin console, expire in 1–90 days.
  - **OAuth client credentials** (`tskey-client-...`) — machine-scoped (`devices:read`, `devices:write`, `dns:read`,
    others), exchanged for short-lived access tokens.
  - **Trust credentials** — delegated fine-grained access limited by key/value claims.
- **Body**: JSON; HuJSON (comments + trailing commas) for the ACL endpoint.
- **No pagination** (lists return everything); no published rate limits, but back off on 429s.

## Canonical shapes

### Authenticate

```bash
curl -u "$TOKEN:" https://api.tailscale.com/api/v2/tailnet/-/devices                      # Basic, any token
curl -H "Authorization: Bearer $TOKEN" https://api.tailscale.com/api/v2/tailnet/-/devices # Bearer
curl -X POST https://api.tailscale.com/api/v2/oauth/token \
  -d "client_id=$CLIENT_ID" -d "client_secret=$CLIENT_SECRET" -d "grant_type=client_credentials"
```

### Device endpoints

| Call | Purpose |
|---|---|
| `GET /tailnet/-/devices` | List; fields include `id`, `hostname`, `addresses`, `tags`, `os`, `lastSeen` (ISO timestamp) |
| `GET` / `DELETE /device/{id}` | Get or delete one device |
| `POST /device/{id}/authorized` `{"authorized": true}` | Authorize / deauthorize |
| `POST /device/{id}/expire` | Expire the device key (force re-auth) |
| `POST /device/{id}/tags` `{"tags": [...]}` | Replace tags |

### Create an auth key

```bash
curl -X POST -u "$TOKEN:" -H "Content-Type: application/json" \
  -d '{"capabilities":{"devices":{"create":{"reusable":true,"ephemeral":true,"preauthorized":true,
       "tags":["tag:ci"]}}},"expirySeconds":86400,"description":"CI runner key"}' \
  https://api.tailscale.com/api/v2/tailnet/-/keys
```

The key value is returned **only once**.

### Update the policy file (ACL)

```bash
curl -u "$TOKEN:" https://api.tailscale.com/api/v2/tailnet/-/acl     # HuJSON + ETag header
curl -X POST -u "$TOKEN:" -H "Content-Type: application/json" -H "If-Match: \"$ETAG\"" \
  -d @policy.json https://api.tailscale.com/api/v2/tailnet/-/acl
curl -X POST -u "$TOKEN:" -H "Content-Type: application/json" \
  -d '{"src":"alice@example.com","dst":"tag:server"}' https://api.tailscale.com/api/v2/tailnet/-/acl/preview
```

## Automation patterns

- **Atomic policy updates**: pass `If-Match` with the GET `ETag`; without it a concurrent edit is silently overwritten.
- **Bulk delete stale devices**: list, filter by `lastSeen`, `DELETE /device/{id}`; throttle for 429s.
- **Device ID**: locally, `tailscale status --json` beats an API round-trip.
- **Programmatic approval**: webhook on `nodeNeedsApproval` → check external state → `POST /device/{id}/authorized`.

## Where to find current information

Fetch rather than reconstruct endpoint specifics, OAuth scope names, webhook payloads/signatures and log fields.

| Topic | Fetch |
|---|---|
| Full REST reference | <https://tailscale.com/docs/reference/tailscale-api> |
| OAuth clients (scopes, secrets, ephemeral flag) | <https://tailscale.com/docs/features/oauth-clients> |
| Trust credentials | <https://tailscale.com/docs/reference/trust-credentials> |
| Webhooks (events, payloads) | <https://tailscale.com/docs/features/webhooks> |
| Rotate a webhook signing secret | <https://tailscale.com/docs/features/webhooks/how-to/rotate-webhook-secret> |
| Logging overview | <https://tailscale.com/docs/features/logging> |
| Configuration audit logs | <https://tailscale.com/docs/features/logging/audit-logging> |
| Network flow logs | <https://tailscale.com/docs/features/logging/network-flow-logs> |
| Log streaming (SIEM, S3) | <https://tailscale.com/docs/features/logging/log-streaming> |
| Streaming event schema | <https://tailscale.com/docs/reference/logging-streaming-events> |
| `tsnet` Server API | <https://tailscale.com/docs/reference/tsnet-server-api> |
