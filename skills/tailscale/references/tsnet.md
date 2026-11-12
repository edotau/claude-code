# tsnet — embed Tailscale in a Go program

`tsnet` lets a Go program join a tailnet as its own device — own Tailscale IP, MagicDNS name and policy identity —
so the application, not the host, is the tailnet member. It is a general networking library, not just an HTTP
server: tailnet listeners for TCP, TLS, UDP and SSH, outbound `Dial`/`HTTPClient`, and the full LocalAPI. Typical
uses: several services on one host with separate identities and rules, client-only programs reaching private
services, ephemeral workers (`Server.Ephemeral = true`), and fronting a non-Go backend as a Tailscale Service.

> tsnet is **Go-only**. For other languages, run the regular `tailscaled` daemon (often as a sidecar).

## Mental model

A `tsnet.Server` is one tailnet node. Set fields (hostname, auth, state dir, tags), then call `Start()` — or call
`Listen()` / `HTTPClient()` / `LocalClient()`, which start it implicitly. Each `Server` keeps its own state directory
(default: OS user configuration directory, `tsnet-<binary>` subdirectory); multiple `Server` instances in one process
give one binary multiple identities, each with a distinct `Dir`. Listeners are plain `net.Listener` values, so any
`net/http`, gRPC or raw-TCP code works on top.

**There is no sign-in flow: the WireGuard tunnel is the identity.** In a handler, `lc.WhoIs(ctx, r.RemoteAddr)`
resolves the peer to a tailnet user, node and capability map — that is your authorization basis.

## Hello, tsnet

A minimal HTTP server that joins the tailnet and identifies the caller via `LocalClient.WhoIs`.

```go
// tshello.go
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"

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
		fmt.Fprintf(w, "<h1>Hello, world!</h1><p>You are <b>%s</b> from <b>%s</b> (%s)</p>",
			html.EscapeString(who.UserProfile.LoginName),
			html.EscapeString(firstLabel(who.Node.ComputedName)),
			r.RemoteAddr)
	})))
}

func firstLabel(s string) string { s, _, _ = strings.Cut(s, "."); return s }
```

`go get tailscale.com/tsnet`, then `go run .`: the first run logs an auth URL to add the device; then
`curl http://tshello` from any tailnet device. `srv.ListenTLS("tcp", ":443")` replaces the `tls.NewListener` block
with an auto-provisioned cert; `srv.ListenFunnel("tcp", ":443")` additionally exposes it publicly via Funnel.

## Authenticating the app to the tailnet

Four ways to authenticate a `tsnet.Server`, in increasing order of automation:

| Method | Set via | Use when |
|---|---|---|
| Interactive auth URL | (no auth set) | Local development; you're fine clicking a login link on first start |
| **Auth key** | `Server.AuthKey` or `TS_AUTHKEY` environment variable | Most servers and containers — quick to issue and revoke |
| **OAuth client** | `Server.ClientSecret` (+ `Server.AdvertiseTags`) or `TS_CLIENT_SECRET` environment variable | Fleets / long-running deployments where you want scoped, rotatable credentials and auto-minted auth keys |
| **Workload identity (OIDC)** | `Server.ClientID` + `Server.IDToken` or environment variables `TS_CLIENT_ID` + `TS_ID_TOKEN` (+ `Server.AdvertiseTags`), plus a blank import (refer to the workload identity section below) | Running in GCP, Azure, or GitHub Actions and want no static secret. The cloud provider's OIDC token is exchanged for an auth key |

`Server.AuthKey` takes precedence over `TS_AUTHKEY`, which takes precedence over the legacy `TS_AUTH_KEY`. OAuth and
workload-identity flows **require** `Server.AdvertiseTags`: the minted auth key is tag-scoped, so untagged tsnet nodes
can't be created this way. The OAuth client needs the `auth_keys` write scope and a tag in `AdvertiseTags`; tsnet
exchanges the client secret for a short-lived auth key on each `Start`.

Auth keys (admin console **Settings → Keys**) for a long-running server: tag it (such as `tag:myapp`) and **don't**
mark it ephemeral, so the device persists across restarts. For short-lived workers set `Server.Ephemeral = true`
instead; the device is cleaned up after disconnect.

### Workload identity (OIDC)

Workload identity federation is **not linked into tsnet by default**. Without this blank import, tsnet silently
ignores `ClientID`, `IDToken` and `Audience` and continues to the next auth method:

```go
import _ "tailscale.com/feature/identityfederation"
```

Instead of supplying `IDToken`, set `Server.Audience` (or `TS_AUDIENCE`) and tsnet requests the ID token from the
cloud provider before exchanging it; refer to the workload-identity-federation docs for the discovery flow.

### Persistent state directory

State (machine key, node key, peer info) lives in `Server.Dir`, which must already exist; override the default when
running as a service or in a container. Losing it re-registers the node as a new device on next start, so mount a
persistent volume there (and at any app database file). After the first auth-key boot writes state, restarts
reconnect without the auth key.

### Optional but recommended

- `hostinfo.SetApp("myapp")` before `Start()` — surfaces your app name in admin-console `Hostinfo`.
- `srv.Logf = func(string, ...any) {}` — default logging is noisy; silence it and add a verbose flag.
- `srv.Up(ctx)` after `Start()` — blocks until the node is online; use before `LocalClient.Status` or serving.
- `srv.ControlURL = "https://control.example.com"` (or `TS_CONTROL_URL`) — self-hosted coordination server. Empty
  falls back to the environment variable, then to Tailscale's default.

## Controlling access via tailnet policy

The policy file governs the tsnet node like any device. **Tag the node** (rather than tying it to a user) and write
grants against the tag, for both who reaches the app and what the app can reach (grant syntax:
[access-control.md](access-control.md)). With an auth key, the key must be created with the tag selected; with
OAuth, the client owns the tag implicitly.

```json
{
  "tagOwners": {
    "tag:myapp": ["autogroup:admin"]
  }
}
```

### Application-layer access — capability grants

For app-level authorization (admin vs. read-only, per-team), **don't hard-code it in the binary**: use a capability
grant with a custom name (`tailscale.com/cap/<yourapp>`) and read it from `WhoIs(...).CapMap`, so operators change
roles by editing the policy file, not by redeploying.

Policy file — grant admin to a group:

```json
{
  "grants": [
    {
      "src": ["group:myapp-admins"],
      "dst": ["tag:myapp"],
      "app": {
        "tailscale.com/cap/myapp": [{ "admin": true }]
      }
    }
  ]
}
```

Application code — resolve user and capabilities from the connection:

```go
import (
	"context"

	"tailscale.com/client/local"
	"tailscale.com/tailcfg"
)

const peerCapName = "tailscale.com/cap/myapp"

type myCaps struct {
	Admin bool `json:"admin"`
}

func currentUser(ctx context.Context, lc *local.Client, remoteAddr string) (login string, isAdmin bool, err error) {
	who, err := lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return "", false, err
	}
	login = who.UserProfile.LoginName
	caps, _ := tailcfg.UnmarshalCapJSON[myCaps](who.CapMap, peerCapName)
	for _, c := range caps {
		if c.Admin {
			isAdmin = true
		}
	}
	return login, isAdmin, nil
}
```

For tagged callers (other tsnet services), `who.UserProfile.LoginName` is `"tagged-devices"`, not a user email.
Handle that explicitly for machine-to-machine authorization, and grant capabilities to the calling tag the same way.

## HTTPS, Funnel, and Tailscale Services

`ListenTLS` needs HTTPS enabled in the tailnet (admin console → DNS → HTTPS). Check at startup rather than blindly
opening :443:

```go
status, _ := lc.Status(ctx)
httpsAvailable := status.Self.HasCap(tailcfg.CapabilityHTTPS) && len(srv.CertDomains()) > 0
```

Otherwise fall back to plain `Listen` on :80 or fail clearly; with HTTPS on, redirect :80 → :443 and add HSTS.

`tsnet.FunnelOnly()` makes a public-only Funnel listener, to split private and public logic:

```go
publicLn, _  := srv.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())
privateLn, _ := srv.ListenTLS("tcp", ":443")
```

Funnel requires the `funnel` `nodeAttr` in the policy file: [sharing-and-publishing.md](sharing-and-publishing.md).

`Server.ListenService("svc:name", tsnet.ServiceModeHTTP{HTTPS: true, Port: 443})` registers a **Tailscale
Service** — a stable virtual hostname/VIP backed by one or more tsnet processes. Use it on ephemeral infrastructure
(fly.io, Cloud Run, k8s pods with non-persistent state) where node identities churn but the service identity must
not. Requires devices on v1.86.0+, a tagged node, a service definition, and an auto-approver:

```json
{
  "tagOwners":     { "tag:myapp": ["autogroup:admin"] },
  "autoApprovers": { "services": { "svc:myapp": ["tag:myapp"] } }
}
```

- Untagged node: `ListenService` returns `tsnet.ErrUntaggedServiceHost` — surface it as a clear operator error.
- In service mode the internal proxy injects identity headers (`Tailscale-User-Login`, `X-Forwarded-For`) on
  loopback connections: trust them only when the immediate `RemoteAddr` is loopback, and still authorize via
  `WhoIs` on the forwarded IP.
- One `ListenService` call per port. The listener carries the service `FQDN`; to front an external (non-Go) backend,
  serve a reverse proxy on it:

```go
const targetAddress = "1.2.3.4:80" // the backing server

ln, _ := srv.ListenService("svc:my-service", tsnet.ServiceModeHTTP{HTTPS: true, Port: 443})
log.Printf("Listening on https://%v\n", ln.FQDN)
log.Fatal(http.Serve(ln, httputil.NewSingleHostReverseProxy(&url.URL{
	Scheme: "http",
	Host:   targetAddress,
})))
```

## Useful Server methods

| Method | Purpose |
|---|---|
| `Listen(network, addr)` | Plain `net.Listener` on the tailnet |
| `ListenTLS(network, addr)` | TLS listener with auto-provisioned cert (HTTPS) |
| `ListenFunnel(network, addr, opts...)` | TLS listener exposed publicly via Funnel; `tsnet.FunnelOnly()` for public-only |
| `ListenService(svc, mode)` | Register as a Tailscale Service (stable identity, multi-instance) |
| `ListenSSH(addr)` | Tailscale SSH listener; connections carry peer identity. Needs blank import `_ "tailscale.com/feature/ssh"` or it errors |
| `ListenPacket(network, addr)` | UDP listener returning `net.PacketConn`; network is `udp`/`udp4`/`udp6`, the address needs an explicit IP (from `TailscaleIPs()`) |
| `Dial(ctx, network, addr)` | Outgoing connection from inside the tailnet |
| `HTTPClient()` | `*http.Client` whose transport routes through the tailnet |
| `LocalClient()` | Talks to embedded `tailscaled` — `WhoIs`, `GetCertificate`, `Status`, others. |
| `TailscaleIPs()` | The node's own tailnet IPv4/IPv6 (after `Start`) |
| `Up(ctx)` | Block until the node is online (after `Start`) |
| `CertDomains()` | Domains the node can mint TLS certs for |
| `Start()` / `Close()` | Lifecycle (most methods Start implicitly; always defer Close) |

## Production checklist

1. **Tag the node** — keeps it out of users' personal device lists and makes policy writable.
2. **Persist `Server.Dir`** (and any app database) on a mounted volume, or each restart is a new device.
3. **OAuth or workload identity** for fleets; auth keys suit single instances but rotate poorly.
4. **Authorize from `CapMap`**, not hard-coded user lists.
5. **Check `HasCap(tailcfg.CapabilityHTTPS)` and `CertDomains()`** before serving TLS.
6. **Ephemeral hosts** (fly.io, k8s, serverless): prefer `ListenService`.
7. **Defer `srv.Close()`** in `main` so the device disconnects cleanly.

## Where to find current information

| Topic | Fetch |
|---|---|
| tsnet overview & install | <https://tailscale.com/docs/features/tsnet> |
| Hello tsnet (basic HTTP app) | <https://tailscale.com/docs/features/tsnet/how-to/create-basic-tsnet-app> |
| Register a tsnet app as a Tailscale Service | <https://tailscale.com/docs/features/tsnet/how-to/register-service> |
| `tsnet.Server` field/method reference | <https://tailscale.com/docs/reference/tsnet-server-api> |
| Auth keys (generation, scoping) | <https://tailscale.com/docs/features/access-control/auth-keys> |
| OAuth clients (for tsnet `ClientSecret`) | <https://tailscale.com/docs/features/oauth-clients> |
| Workload identity federation (`ClientID`/`IDToken`) | <https://tailscale.com/docs/features/workload-identity-federation> |
| Userspace networking concepts | <https://tailscale.com/docs/concepts/userspace-networking> |
| Go pkg.go.dev reference | <https://pkg.go.dev/tailscale.com/tsnet> |

## Answering pattern

Match the listener to the protocol (`Listen` TCP, `ListenTLS` HTTPS, `ListenPacket` UDP, `ListenSSH` SSH,
`Dial`/`HTTPClient` outbound) and remember the SSH and workload-identity blank imports. Public exposure → Funnel
(`ListenFunnel`); stable identity across restarts or replicas → `ListenService` and the register-service how-to.
WebFetch `tsnet-server-api` for any exact field name (`Server.Ephemeral`, `Server.ControlURL`, `Server.Audience`,
`Server.UserLogf`, `Server.RunWebClient`, …) or behavior you're unsure of — it is the source of truth and grows over
time; fetch the auth-keys / OAuth pages for the current admin-console flow.
