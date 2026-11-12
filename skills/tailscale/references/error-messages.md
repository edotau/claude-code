# Error and status messages

Tailscale publishes a page for many specific messages: **client messages** (app, `tailscale` CLI, `tailscaled`) and
**console messages** (admin console sign-in or administration). Match the quoted message or health warning to a row
— on paraphrase, match the symptom ("something about a relay server" → the DERP rows) — WebFetch it, and give the
cause and fix from that page. No match: fetch the hub <https://tailscale.com/docs/reference/messages>, since pages are
added over time. Broader connectivity or platform problems: [connectivity.md](connectivity.md).

## Client messages

| If the user sees / reports… | Fetch |
|---|---|
| "Coordination server reports an issue" | <https://tailscale.com/docs/reference/messages/client/coordination-server-issue> |
| "Could not apply configuration" | <https://tailscale.com/docs/reference/messages/client/could-not-apply-config> |
| Docker connectivity broken by stateful filtering | <https://tailscale.com/docs/reference/messages/client/docker-stateful-filtering> |
| "Invalid packet filter" | <https://tailscale.com/docs/reference/messages/client/invalid-packet-filter> |
| Local log misconfiguration | <https://tailscale.com/docs/reference/messages/client/local-log-config-error> |
| "MagicSock function not running" | <https://tailscale.com/docs/reference/messages/client/magicsock-receive-func-error> |
| "Network map response timeout" | <https://tailscale.com/docs/reference/messages/client/network-map-response-timeout> |
| "Network down" / network status warning | <https://tailscale.com/docs/reference/messages/client/network-status> |
| Relay server unavailable (no DERP connection) | <https://tailscale.com/docs/reference/messages/client/no-derp-connection> |
| No home relay server | <https://tailscale.com/docs/reference/messages/client/no-derp-home> |
| "Out of sync" (not in map poll) | <https://tailscale.com/docs/reference/messages/client/not-in-map-poll> |
| Linux DNS configuration issue (`resolv.conf` overwritten) | <https://tailscale.com/docs/reference/messages/client/resolv-conf-overwritten> |
| Tailscale blocked by Screen Time | <https://tailscale.com/docs/reference/messages/client/screen-time-controlclient> |
| "Security update available" | <https://tailscale.com/docs/reference/messages/client/security-update-available> |
| Windows network configuration failed | <https://tailscale.com/docs/reference/messages/client/set-network-category-failed> |
| Tailscale SSH unavailable with SELinux enabled | <https://tailscale.com/docs/reference/messages/client/ssh-unavailable-selinux-enabled> |
| "Encrypted connection failed" (TLS) | <https://tailscale.com/docs/reference/messages/client/tls-connection-failed> |
| "Update available" | <https://tailscale.com/docs/reference/messages/client/update-available> |
| "Using an unstable version" | <https://tailscale.com/docs/reference/messages/client/using-unstable-version> |

## Console messages

| If the user sees / reports… | Fetch |
|---|---|
| "Your account is not an administrator" | <https://tailscale.com/docs/reference/messages/console/account-not-admin> |
| Authentication failed while retrieving details from the identity provider (SSO) | <https://tailscale.com/docs/reference/messages/console/auth-failed-sso> |
| Login name change detected | <https://tailscale.com/docs/reference/messages/console/login-name-change> |
| Multiple users with the same login | <https://tailscale.com/docs/reference/messages/console/multi-user-login> |
| "You don't have access to the admin console" | <https://tailscale.com/docs/reference/messages/console/no-access> |
| "Error 500: no auth service" | <https://tailscale.com/docs/reference/messages/console/no-auth-service> |
| Organization has restricted joining external tailnets | <https://tailscale.com/docs/reference/messages/console/org-has-restrictions> |
| "Reached use limit" | <https://tailscale.com/docs/reference/messages/console/reached-use-limit> |
| "Failed to load sharing information" | <https://tailscale.com/docs/reference/messages/console/sharing-failure> |
