# Error and status messages

Tailscale publishes a page per message: **client messages** (app, `tailscale`, `tailscaled`) and **console messages**
(admin console). Match the quoted message or health warning to a row (on paraphrase, match the symptom — "something
about a relay server" → DERP rows), WebFetch it, and give that page's cause and fix. No match: fetch the hub
<https://tailscale.com/docs/reference/messages> (pages are added over time). Broader problems:
[connectivity.md](connectivity.md).

## Client messages

All under `https://tailscale.com/docs/reference/messages/client/`:

| If the user sees… | Fetch (suffix) |
|---|---|
| "Coordination server reports an issue" | `coordination-server-issue` |
| "Could not apply configuration" | `could-not-apply-config` |
| Docker connectivity broken by stateful filtering | `docker-stateful-filtering` |
| "Invalid packet filter" | `invalid-packet-filter` |
| Local log misconfiguration | `local-log-config-error` |
| "MagicSock function not running" | `magicsock-receive-func-error` |
| "Network map response timeout" | `network-map-response-timeout` |
| "Network down" / network status warning | `network-status` |
| Relay server unavailable (no DERP connection) | `no-derp-connection` |
| No home relay server | `no-derp-home` |
| "Out of sync" (not in map poll) | `not-in-map-poll` |
| Linux DNS issue (`resolv.conf` overwritten) | `resolv-conf-overwritten` |
| Blocked by Screen Time | `screen-time-controlclient` |
| "Security update available" | `security-update-available` |
| Windows network configuration failed | `set-network-category-failed` |
| Tailscale SSH unavailable with SELinux | `ssh-unavailable-selinux-enabled` |
| "Encrypted connection failed" (TLS) | `tls-connection-failed` |
| "Update available" | `update-available` |
| "Using an unstable version" | `using-unstable-version` |

## Console messages

All under `https://tailscale.com/docs/reference/messages/console/`:

| If the user sees… | Fetch (suffix) |
|---|---|
| "Your account is not an administrator" | `account-not-admin` |
| SSO authentication failed retrieving identity provider details | `auth-failed-sso` |
| Login name change detected | `login-name-change` |
| Multiple users with the same login | `multi-user-login` |
| "You don't have access to the admin console" | `no-access` |
| "Error 500: no auth service" | `no-auth-service` |
| Organization restricts joining external tailnets | `org-has-restrictions` |
| "Reached use limit" | `reached-use-limit` |
| "Failed to load sharing information" | `sharing-failure` |
