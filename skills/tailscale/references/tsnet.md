# tsnet — embed Tailscale in a Go program

`tsnet` lets a Go program join a tailnet as its own device (own Tailscale IP, MagicDNS name, policy identity). It is a
general networking library: tailnet listeners for TCP, TLS, UDP and SSH, outbound `Dial`/`HTTPClient`, and the full
LocalAPI. Uses: several services on one host with separate identities, client-only programs, ephemeral workers
(`Server.Ephemeral = true`), fronting a non-Go backend as a Tailscale Service.

> **Go-only.** Other languages: run `tailscaled` (often as a sidecar).

## Mental model

A `tsnet.Server` is one tailnet node. Set fields (hostname, auth, state dir, tags), then `Start()` — or call
`Listen()` / `HTTPClient()` / `LocalClient()`, which start it implicitly. Each `Server` has its own state directory
(default: OS user config dir, `tsnet-<binary>` subdirectory); several `Server`s in one process give several
identities, each with a distinct `Dir`. Listeners are plain `net.Listener`s, so `net/http`, gRPC or raw TCP work.

**There is no sign-in flow: the WireGuard tunnel is the identity.** `lc.WhoIs(ctx, r.RemoteAddr)` resolves the peer
to a tailnet user, node and capability map — your authorization basis.

## Hello, tsnet

```go
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"

	"tailscale.com/tsnet"
)

var addr = flag.String("addr", ":80", "address to listen on")

func main() {
	flag.Parse()
	srv := new(tsnet.Server)
	srv.Hostname = "tshello"
	defer srv.Close()

	ln, err := srv.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	lc, err := srv.LocalClient()
	if err != nil {
		log.Fatal(err)
	}
	if *addr == ":443" {
		ln = tls.NewListener(ln, &tls.Config{GetCertificate: lc.GetCertificate})
	}

	log.Fatal(http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, err := lc.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "Hello, %s", html.EscapeString(who.UserProfile.LoginName))
	})))
}
```

`go get tailscale.com/tsnet`, `go run .`: the first run logs an auth URL; then `curl http://tshello` from a tailnet
device. `srv.ListenTLS("tcp", ":443")` replaces the `tls.NewListener` block with an auto-provisioned cert;
`srv.ListenFunnel("tcp", ":443")` also exposes it publicly.

## Authenticating the app to the tailnet

| Method | Set via | Use when |
|---|---|---|
| Interactive auth URL | (no auth set) | Local development |
| **Auth key** | `Server.AuthKey` or `TS_AUTHKEY` | Most servers/containers |
| **OAuth client** | `Server.ClientSecret` (+ `Server.AdvertiseTags`) or `TS_CLIENT_SECRET` | Fleets: scoped, rotatable credentials, auto-minted auth keys |
| **Workload identity (OIDC)** | `Server.ClientID` + `Server.IDToken` or `TS_CLIENT_ID` + `TS_ID_TOKEN` (+ `Server.AdvertiseTags`), plus a blank import (below) | GCP, Azure, GitHub Actions without a static secret; the cloud OIDC token is exchanged for an auth key |

`Server.AuthKey` beats `TS_AUTHKEY`, which beats legacy `TS_AUTH_KEY`. OAuth and workload identity **require**
`Server.AdvertiseTags` (the minted key is tag-scoped; untagged nodes can't be created this way). The OAuth client
needs the `auth_keys` write scope and a tag in `AdvertiseTags`; tsnet exchanges the secret for a short-lived auth key
on each `Start`.

Auth keys (**Settings → Keys**) for a long-running server: tag it (e.g. `tag:myapp`) and **don't** mark it ephemeral.
Short-lived workers: `Server.Ephemeral = true` (cleaned up after disconnect).

### Workload identity (OIDC)

Not linked into tsnet by default: without this blank import tsnet silently ignores `ClientID`, `IDToken` and
`Audience` and tries the next auth method:

```go
import _ "tailscale.com/feature/identityfederation"
```

Instead of `IDToken`, set `Server.Audience` (or `TS_AUDIENCE`) and tsnet requests the token from the cloud provider.

### Persistent state directory

State (machine key, node key, peers) lives in `Server.Dir`, which must already exist; override the default when
running as a service or container. Losing it registers a new device on next start, so mount a persistent volume
(and at any app database). After the first auth-key boot, restarts reconnect without the key.

### Optional but recommended

- `hostinfo.SetApp("myapp")` before `Start()` — app name in admin-console `Hostinfo`.
- `srv.Logf = func(string, ...any) {}` — silence noisy default logging.
- `srv.Up(ctx)` after `Start()` — blocks until online; use before `LocalClient.Status` or serving.
- `srv.ControlURL = "https://control.example.com"` (or `TS_CONTROL_URL`) — self-hosted coordination server; empty
  falls back to the env var, then Tailscale's default.

## Controlling access via tailnet policy

The policy file governs the node like any device. **Tag the node** and write grants against the tag, for both who
reaches the app and what it reaches ([access-control.md](access-control.md)). Auth key: create it with the tag
selected; OAuth: the client owns the tag implicitly.

```json
{ "tagOwners": { "tag:myapp": ["autogroup:admin"] } }
```

### Application-layer access — capability grants

For app-level authorization (admin vs. read-only, per team), **don't hard-code it**: grant a custom capability
(`tailscale.com/cap/<yourapp>`) and read it from `WhoIs(...).CapMap`, so operators change roles in the policy file.

```json
{
  "grants": [{
    "src": ["group:myapp-admins"],
    "dst": ["tag:myapp"],
    "app": { "tailscale.com/cap/myapp": [{ "admin": true }] }
  }]
}
```

```go
const peerCapName = "tailscale.com/cap/myapp"

type myCaps struct {
	Admin bool `json:"admin"`
}

func currentUser(ctx context.Context, lc *local.Client, remoteAddr string) (login string, isAdmin bool, err error) {
	who, err := lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return "", false, err
	}
	caps, _ := tailcfg.UnmarshalCapJSON[myCaps](who.CapMap, peerCapName)
	for _, c := range caps {
		isAdmin = isAdmin || c.Admin
	}
	return who.UserProfile.LoginName, isAdmin, nil
}
```

(imports: `tailscale.com/client/local`, `tailscale.com/tailcfg`.) For tagged callers (other tsnet services)
`who.UserProfile.LoginName` is `"tagged-devices"`, not an email; handle that for machine-to-machine authorization and
grant capabilities to the calling tag the same way.

## HTTPS, Funnel, and Tailscale Services

`ListenTLS` needs HTTPS enabled in the tailnet (admin console → DNS → HTTPS). Check at startup:

```go
status, _ := lc.Status(ctx)
httpsAvailable := status.Self.HasCap(tailcfg.CapabilityHTTPS) && len(srv.CertDomains()) > 0
```

If unavailable, fall back to plain `Listen` on :80 or fail clearly; with HTTPS, redirect :80 → :443 and add HSTS.

`tsnet.FunnelOnly()` makes a public-only Funnel listener:

```go
publicLn, _  := srv.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())
privateLn, _ := srv.ListenTLS("tcp", ":443")
```

Funnel needs the `funnel` `nodeAttr`: [sharing-and-publishing.md](sharing-and-publishing.md).

`Server.ListenService("svc:name", tsnet.ServiceModeHTTP{HTTPS: true, Port: 443})` registers a **Tailscale Service** —
a stable virtual hostname/VIP backed by one or more tsnet processes, for ephemeral infrastructure (fly.io, Cloud Run,
k8s pods) where node identities churn. Needs devices on v1.86.0+, a tagged node, a service definition and an
auto-approver:

```json
{
  "tagOwners":     { "tag:myapp": ["autogroup:admin"] },
  "autoApprovers": { "services": { "svc:myapp": ["tag:myapp"] } }
}
```

- Untagged node: `ListenService` returns `tsnet.ErrUntaggedServiceHost` — surface it as an operator error.
- The service proxy injects identity headers (`Tailscale-User-Login`, `X-Forwarded-For`) on loopback connections:
  trust them only when the immediate `RemoteAddr` is loopback, and still authorize via `WhoIs` on the forwarded IP.
- One `ListenService` call per port. The listener has the service `FQDN`; to front a non-Go backend, serve a reverse
  proxy on it:

```go
ln, _ := srv.ListenService("svc:my-service", tsnet.ServiceModeHTTP{HTTPS: true, Port: 443})
log.Printf("Listening on https://%v\n", ln.FQDN)
log.Fatal(http.Serve(ln, httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "1.2.3.4:80"})))
```

## Useful Server methods

| Method | Purpose |
|---|---|
| `Listen(network, addr)` | Plain `net.Listener` on the tailnet |
| `ListenTLS(network, addr)` | TLS with auto-provisioned cert |
| `ListenFunnel(network, addr, opts...)` | Public via Funnel; `tsnet.FunnelOnly()` for public-only |
| `ListenService(svc, mode)` | Tailscale Service (stable identity, multi-instance) |
| `ListenSSH(addr)` | Tailscale SSH listener with peer identity; needs blank import `_ "tailscale.com/feature/ssh"` or it errors |
| `ListenPacket(network, addr)` | UDP `net.PacketConn`; `udp`/`udp4`/`udp6`, address needs an explicit IP (from `TailscaleIPs()`) |
| `Dial(ctx, network, addr)` | Outgoing connection from inside the tailnet |
| `HTTPClient()` | `*http.Client` routed through the tailnet |
| `LocalClient()` | Embedded `tailscaled` client — `WhoIs`, `GetCertificate`, `Status`, others |
| `TailscaleIPs()` | Own tailnet IPv4/IPv6 (after `Start`) |
| `Up(ctx)` | Block until online (after `Start`) |
| `CertDomains()` | Domains the node can mint certs for |
| `Start()` / `Close()` | Lifecycle (most methods Start implicitly; always defer Close) |

## Production checklist

1. **Tag the node** — out of personal device lists, policy writable.
2. **Persist `Server.Dir`** (and any app database), or each restart is a new device.
3. **OAuth or workload identity** for fleets; auth keys rotate poorly.
4. **Authorize from `CapMap`**, not hard-coded user lists.
5. **Check `HasCap(tailcfg.CapabilityHTTPS)` and `CertDomains()`** before serving TLS.
6. **Ephemeral hosts** (fly.io, k8s, serverless): prefer `ListenService`.
7. **Defer `srv.Close()`** so the device disconnects cleanly.

## Where to find current information

| Topic | Fetch |
|---|---|
| Overview & install | <https://tailscale.com/docs/features/tsnet> |
| Hello tsnet | <https://tailscale.com/docs/features/tsnet/how-to/create-basic-tsnet-app> |
| Register a Tailscale Service | <https://tailscale.com/docs/features/tsnet/how-to/register-service> |
| `tsnet.Server` field/method reference | <https://tailscale.com/docs/reference/tsnet-server-api> |
| Auth keys | <https://tailscale.com/docs/features/access-control/auth-keys> |
| OAuth clients (`ClientSecret`) | <https://tailscale.com/docs/features/oauth-clients> |
| Workload identity federation | <https://tailscale.com/docs/features/workload-identity-federation> |
| Userspace networking | <https://tailscale.com/docs/concepts/userspace-networking> |
| pkg.go.dev | <https://pkg.go.dev/tailscale.com/tsnet> |

## Answering pattern

Match listener to protocol (`Listen` TCP, `ListenTLS` HTTPS, `ListenPacket` UDP, `ListenSSH` SSH, `Dial`/`HTTPClient`
outbound); remember the SSH and workload-identity blank imports. Public → `ListenFunnel`; stable identity across
restarts/replicas → `ListenService`. WebFetch `tsnet-server-api` for any exact field (`Server.Ephemeral`,
`ControlURL`, `Audience`, `UserLogf`, `RunWebClient`, …) — it is the source of truth; fetch the auth-keys/OAuth pages
for the current admin-console flow.
