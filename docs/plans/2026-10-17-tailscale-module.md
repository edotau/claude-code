# Tailscale module for the harness — findings and plan

Date: 2026-10-17. Status: research + proposal, nothing implemented. Probes built in the session scratchpad
(`tsnet-probe`, `local-probe`, `api-probe`), not in the repo.

## 1. What is on this box today (live)

| Fact | Value |
| --- | --- |
| Client | `Tailscale.app` 1.104.1 (GUI variant); `/usr/local/bin/tailscale` is a 2-line wrapper into the app bundle |
| Daemon reachability | no `/var/run/tailscaled.socket`; LocalAPI is `http://localhost:<port>` + a per-user token (`tailscale debug local-creds`) — `tailscale.com/client/local` finds it on its own (verified by the probe below) |
| Tailnet | `tailbb611b.ts.net`, MagicDNS on; this account is owner + admin |
| Nodes | `erics-macbook-pro` (100.107.35.117), `iphone181` (iOS, online) |
| Exit node | **already advertised and approved**: `ExitNodeOption=true`, `AllowedIPs` carries `0.0.0.0/0, ::/0` |
| Tags / serve | no tags on the Mac; `tailscale serve status` = no config |
| Caps on self | file-sharing, is-admin, is-owner, ssh, tailnet-lock |

Homebrew: cask `tailscale-app` 1.102.4 (what is installed, auto-updates) and formula `tailscale` 1.102.5 (open-source
`tailscaled`, needs a root daemon). Stay on the cask on macOS.

## 2. Three ways in, measured

All three are `tailscale.com` module code (v1.104.1); the harness is stdlib-first with each external SDK boxed in one
package, so weight decides the tier. Stripped `-s -w` builds, go1.27.2, arm64:

| Tier | Import | Binary | Packages | go.mod requires | What it gives |
| --- | --- | --- | --- | --- | --- |
| A — control plane | `tailscale.com/client/tailscale/v2` (separate module, v2.12.0) | 6.4 MB | 202 | 3 (hujson, oauth2) | admin API: devices, keys, ACL policy, DNS, services; approve routes/exit nodes; mint auth keys |
| B — LocalAPI | `tailscale.com/client/local` | 8.2 MB | 323 | 23 | talk to the **installed** daemon: `Status`, `WhoIs`, `EditPrefs` (advertise exit node), `SetServeConfig`, `CertPair`, `Ping`, `IDToken` |
| C — embedded node | `tailscale.com/tsnet` | 20.1 MB | 559 | 44 (gvisor, wireguard-go) | the harness **is** a tailnet node: own IP/hostname/identity per process, `Listen`/`Dial`/`ListenTLS`/`ListenFunnel`/`ListenService`, no daemon needed |

The harness binary is 20 MB unstripped today; tier C roughly doubles it, and brings a userspace network stack into a
CLI whose job is config and routing. Tier B's probe already did the useful things against the running app:

```
backend: Running self: erics-macbook-pro.tailbb611b.ts.net. [100.107.35.117 fd7a:...]
peer: iphone181.tailbb611b.ts.net. true iOS
whois self: ericdotau@gmail.com erics-macbook-pro
```

tsnet limits that matter here: userspace only, so a tsnet node **cannot be an exit node or subnet router**; one
`Server` = one node with its own state dir (`Dir`, default under `os.UserConfigDir()/<binary>`), its own auth
(auth key, OAuth client secret, or an interactive login URL printed through `UserLogf`).

## 3. Recommendation

**Decision (user, 2026-10-17): no `tailscale.com` import at all — a stdlib package.** The dependency footprint of
every tier is out of proportion to the need, which is three things: create (mint an auth key), connect (bring a
machine onto the tailnet), check (what machines are there, are they online). The REST API is plain JSON over
`net/http`, and the installed `tailscale` CLI already does connect/status with `--json`. The measured tiers above
stay as the record of why; tsnet is the §6 digest only, for a later per-agent-identity need.

### 3.1 Package `internal/tailscale` (stdlib only, exec of the installed CLI)

```
internal/tailscale/
  api.go      // Client{tailnet, token}: GET /api/v2/tailnet/-/devices, POST /keys, POST /device/{id}/routes
  token.go    // OAuth client-credentials: POST https://api.tailscale.com/api/v2/oauth/token (1 h, cached in-process)
  cli.go      // exec `tailscale status --json`, `tailscale up --auth-key=… [--advertise-exit-node]`, `tailscale set`
  doctor.go   // one line for `claude-code doctor`: cli found, backend state, node, exit-node, api credential
```

Public surface (the "create / connect / check" the user asked for):

| Func | Does | Via |
| --- | --- | --- |
| `CreateAuthKey(ctx, Opts{Ephemeral, Preauthorized, Reusable, Tags, Expiry}) (key, error)` | mint `tskey-auth-…` for a new machine | API `POST /keys` |
| `Connect(ctx, authKey string, exitNode bool) error` | bring this box onto the tailnet; idempotent when already Running | `tailscale up --auth-key=… [--advertise-exit-node]` |
| `Machines(ctx) ([]Machine, error)` | name, addresses, OS, online/last-seen, tags, exit-node advertised/approved | API `GET /devices` merged with `tailscale status --json` peers |
| `Status(ctx) (Local, error)` | this node: backend state, DNS name, IPs, exit-node option + approved routes | `tailscale status --json` |
| `ApproveExitNode(ctx, deviceID) error` | approve `0.0.0.0/0, ::/0` without the admin console | API `POST /device/{id}/routes` |

Everything else in §3.2–§3.7 (verb shape, `configure` row, Makefile, exit node, mobile, security) stands; where it
names a `tailscale.com` type, read the stdlib equivalent above. Pure-API exit-node approval and key minting need
the admin credential; connect and status need only the CLI.

Credential names (`internal/secrets` names index, keyring-backed): `TAILSCALE_OAUTH_CLIENT_ID` +
`TAILSCALE_OAUTH_CLIENT_SECRET` (preferred: scopes `devices:core` + `auth_keys`, tagged) or `TAILSCALE_API_KEY`
(90-day personal key, used as a bearer directly). OAuth access tokens live 1 h; the package re-mints on 401.

### 3.2 Verb `claude-code tailscale`

| Sub-verb | Does | Backed by |
| --- | --- | --- |
| `status [--format text\|json]` | node, tailnet, peers, exit-node state, serve state | B |
| `whois <ip>` | login + node for a tailnet address (the agent-identity primitive) | B |
| `exit-node [--enable\|--disable] [--approve]` | idempotent advertise; `--approve` calls the admin API when `AllowedIPs` lacks the default routes | B + A |
| `serve <port> [--off]` | publish the router or a harness HTTP surface at `https://<node>.tailbb611b.ts.net` | B |
| `doctor` | the same line `claude-code doctor` prints | B + A |

`exit-node --enable` today would be a no-op on this Mac (already on + approved) — the verb's value is the fresh-box
path and the explicit `--approve` so the phone-side step never needs the admin console.

### 3.3 Auth follows the `configure` pattern (user's ask)

Add a `tailscale` row to `configure`'s **keys** section as a `keyTarget`, exactly like the codex/copilot/gemini rows
in `vendorKeyTarget` (`internal/configure/configure.go`):

| `keyTarget` field | tailscale row |
| --- | --- |
| `present` / `source` | daemon logged in → `"tailscale login (" + Self.DNSName + ")"`; checked via `local.Client.Status` (`BackendState == "Running"`) |
| `loginArgv` | `["tailscale", "login"]` (browser flow; the GUI app opens the URL itself) — the **Browser login** option |
| `envVar` | `TAILSCALE_OAUTH_CLIENT_SECRET` — the **Paste API key** option, stored via `secrets.Set` (keyring, `--file` fallback) |
| `consoleURLs[envVar]` | `https://login.tailscale.com/admin/settings/oauth` |
| `provider` / `probe` | not a `providers.Provider`; add a small probe hook: `Devices().List` with a 4 s timeout so a stored-but-revoked secret is not "present" (mirrors `providers.ProbeCredential`) |
| `fixCmd` (piped) | `tailscale login · claude-code secrets set TAILSCALE_OAUTH_CLIENT_SECRET` |

Two credentials, two states, one row: "login" is the node (needed for B), "key" is the admin API (needed only for
`--approve`, key minting, policy). Present = login; the row detail says `"; no admin credential"` when the key is
absent, the same shape as the anthropic row's `"; no headless token"`. `configure --check` reports both without
prompting. `configure tailscale` runs just that row (add `"tailscale"` to `Sections` after `keys`, or fold the row
into `keys` and expose it through `--vendor tailscale` like `configure.Vendor`).

### 3.4 Makefile

```make
.PHONY: tailscale-cli
tailscale-cli:  ## Install the Tailscale client (darwin: brew cask; linux: official script); login is `configure`
	@if command -v tailscale >/dev/null 2>&1; then echo "$(C_OK)tailscale found: $$(tailscale version | head -1)"; exit 0; fi; \
	case "$$(uname -s)" in \
	  Darwin) brew install --cask tailscale-app ;; \
	  Linux)  sh -c "$$(curl -fsSL https://tailscale.com/install.sh)" ;; \
	  *) echo "install tailscale manually: https://tailscale.com/download"; exit 1 ;; \
	esac
```

Wire it into `bootstrap` behind a flag (`TAILSCALE=1 make bootstrap`), not unconditionally: a fresh box that only
wants the harness should not get a VPN client. The Linux branch runs a remote script, which the `go-toolchain`
recipe deliberately avoids (checksummed tarball); the distro-package alternative is per-distro apt/dnf repo setup —
longer, but checksum-clean. User's call. Note: the harness safety hook blocks the `curl … | sh` spelling even inside
quoted text, hence the `sh -c "$(curl …)"` form above.

`tailscale login` / `tailscale up` are interactive (browser) and belong in `configure`, never in make.

### 3.5 Exit node in Go (user's ask)

The Go side cannot *be* an exit node (tsnet is userspace). It drives the installed daemon:

1. `EditPrefs(ctx, &ipn.MaskedPrefs{Prefs: ipn.Prefs{AdvertiseRoutes: [0.0.0.0/0, ::/0]}, AdvertiseRoutesSet: true})`
   — equivalent to `tailscale set --advertise-exit-node` (present in this build's `tailscale set --help`).
2. Poll `Status` until `Self.AllowedIPs` contains both default routes (approved) — or, with an admin credential,
   `Devices().SetSubnetRoutes(ctx, deviceID, routes)` approves it from the harness.
3. Report: advertised / approved / in use by `<peer>` (`Status.Peer[*].ExitNode`).

On macOS the GUI app may also require the "Allow local network access"/exit-node toggle; the LocalAPI path worked
for `tailscale set`, which is the same channel.

### 3.6 Mobile (user's ask) — what Tailscale does and does not add

- **Claude Code Remote Control** (the Claude mobile app driving a session) runs through Anthropic's relay. The
  tailnet is not in that path, and nothing here changes the two things that gate it: a `/login` credential and no
  foreign `ANTHROPIC_BASE_URL` (decisionLog 2026-10-17). Do not route it over Tailscale.
- What the phone gains from the tailnet: (1) the exit node — already on, so the phone browses through the Mac's
  network; (2) SSH into the Mac from the phone (Termius, Blink) at `erics-macbook-pro.tailbb611b.ts.net` to launch
  `claude` directly — needs macOS Remote Login; Tailscale's own SSH server is Linux + open-source-tailscaled only, so
  it is sshd over the tailnet IP here (verify on the KB page before relying on it); (3) any harness HTTP surface
  (router status, a future web UI) published with `serve`, reached from the phone over HTTPS with the user's
  identity attached.

### 3.7 Security notes

- `tailscale serve` adds `Tailscale-User-Login` / `Tailscale-User-Name` to proxied requests and strips spoofed
  copies; absent for tagged devices and for Funnel (public) traffic. A served router could accept tailnet identity
  **in addition to** `router.secret`, never instead of it, until the ACL is written.
- Never `ListenFunnel` / `tailscale funnel` a harness surface: it is the public internet.
- `internal/router/netguard.go` `rejectLocal` blocks loopback/private/link-local upstreams; `100.64.0.0/10` (CGNAT,
  the tailnet range) is **not** `IsPrivate()`. A gateway `base_url` that resolves into the tailnet is allowed today.
  Decide whether that is a feature (a tailnet-hosted LiteLLM) or a gap, before serving the router on the tailnet.
- OAuth client secrets go in the keyring (`secrets set`), never `providers.json`; `secrets scan` should learn the
  `tskey-client-` / `tskey-api-` / `tskey-auth-` prefixes.
- Audit log (`internal/audit`): record `exit-node enable/approve`, `serve on/off`, `api credential set` by name.

## 4. Phases

1. `internal/tailscale` stdlib package: `Status`, `Machines`, `Connect`, `CreateAuthKey`, `ApproveExitNode` +
   `claude-code tailscale status|machines|connect|key|exit-node` + `doctor` line + `configure` row + Makefile target.
   Live check: `tailscale status` vs `claude-code tailscale status` on this Mac; `machines` lists the iPhone;
   `exit-node --enable` is a no-op here (already on + approved); `key` needs the OAuth client in the keyring.
2. `secrets scan` learns `tskey-*` prefixes; audit entries for key mint / exit-node / connect.
3. Later, if wanted: `serve` for the router (CLI `tailscale serve`), the netguard CGNAT decision, identity-aware
   router auth from the `Tailscale-User-Login` header.
4. tsnet only if per-agent tailnet identity becomes a requirement (one `Server` per agent, `Ephemeral: true`,
   `AuthKey` minted via `CreateAuthKey`, `AdvertiseTags: [tag:agent]`).

## 5. Open decisions (user)

- `bootstrap` gating: `TAILSCALE=1` flag vs always-on.
- Linux install: official `install.sh` vs distro repo setup.
- `configure`: own section `tailscale` vs a row inside `keys`.
- `Machines`: API only (needs the admin credential, sees every device) vs CLI only (no credential, sees peers this
  node can reach) vs merged. Recommendation: CLI when no credential, merged when there is one.

## 6. tsnet.Server API digest (tailscale.com/tsnet v1.104.1)

Fields: `Dir string` (state dir, default `os.UserConfigDir()/<binary>`), `Store ipn.StateStore` (default FileStore
`Dir/tailscaled.state`), `Hostname string` (default binary name), `Ephemeral bool`, `AuthKey string` (beats env),
`ClientSecret string` (OAuth client secret → auth key), `ClientID`/`IDToken`/`Audience string` (workload identity
federation), `ControlURL string`, `Logf logger.Logf` (verbose), `UserLogf logger.Logf` (auth URL; default
`log.Printf`), `RunWebClient bool` (port 5252), `Port uint16` (WireGuard UDP, 0 = auto), `AdvertiseTags []string`,
`Tun tun.Device`.

Methods: `Start() error` · `Up(ctx) (*ipnstate.Status, error)` · `Close() error` · `Listen(network, addr)
(net.Listener, error)` · `ListenTLS` · `ListenFunnel(network, addr, opts ...FunnelOption)` (`FunnelOnly()`,
`FunnelTLSConfig(*tls.Config)`) · `ListenPacket` · `ListenSSH(addr)` · `ListenService(name string, mode ServiceMode)
(*ServiceListener, error)` (`ServiceModeTCP{Port, TerminateTLS, PROXYProtocolVersion}`, `ServiceModeHTTP{Port,
HTTPS, AcceptAppCaps, PROXYProtocol}`) · `Dial(ctx, network, addr) (net.Conn, error)` · `HTTPClient() *http.Client`
· `LocalClient() (*local.Client, error)` · `TailscaleIPs() (ip4, ip6 netip.Addr)` · `CertDomains() []string` ·
`GetRootPath()` · `Loopback() (addr, proxyCred, localAPICred string, err error)` · `Sys() *tsd.System` ·
`CapturePcap(ctx, file)` · `LogtailWriter() io.Writer` · `RegisterFallbackTCPHandler(cb) func()`.

Env: `TS_AUTHKEY` (preferred) / `TS_AUTH_KEY`, `TS_CLIENT_SECRET`, `TS_CONTROL_URL`, `TSNET_FORCE_LOGIN=1`.

Any `Listen*`/`Dial` calls `Start` implicitly; without a key, `Start` prints a login URL via `UserLogf`. Multiple
`Server`s per process are fine (distinct `Dir` + `Hostname`). Funnel needs HTTPS certs enabled on the tailnet and
the `funnel` node attribute in the policy.

## 7. LocalAPI client digest (tailscale.com/client/local)

`Client{Socket string, Dial func(ctx, network, addr) (net.Conn, error), UseSocketOnly bool, OmitAuth bool,
Transport http.RoundTripper}`; zero value finds the platform daemon, including the macOS GUI app's port+token.
`Status(ctx) (*ipnstate.Status, error)` · `StatusWithoutPeers` · `WhoIs(ctx, "ip[:port]") (*apitype.WhoIsResponse{
Node *tailcfg.Node, UserProfile *tailcfg.UserProfile, CapMap tailcfg.PeerCapMap}, error)` · `WhoIsNodeKey` ·
`GetPrefs` / `EditPrefs(ctx, *ipn.MaskedPrefs) (*ipn.Prefs, error)` · `Start(ctx, ipn.Options)` · `Logout` ·
`GetCertificate(*tls.ClientHelloInfo)` / `CertPair(ctx, domain) (certPEM, keyPEM, err)` · `SetServeConfig` /
`GetServeConfig` · `Ping(ctx, ip, tailcfg.PingType)` · `IDToken(ctx, aud)` · `DaemonMetrics`. API marked
"not necessarily stable between releases" — pin the module version and keep the wrapper thin.

## 8. Control-plane client digest (tailscale.com/client/tailscale/v2 v2.12.0)

`Client{Tailnet string ("-" = the key's tailnet), APIKey string, Auth (OAuth{ClientID, ClientSecret, Scopes} |
IdentityFederation{ClientID, IDTokenFunc}), BaseURL, UserAgent, HTTP *http.Client}`. Resources: `Devices()`
(List/Get/Delete/SetName/SetTags/SetAuthorized/SetSubnetRoutes/SubnetRoutes/SetPostureAttribute/SetIPv4Address/SetKey),
`Keys()` (List/Get/Create/Delete/CreateAuthKey/CreateOAuthClient/CreateFederatedIdentity), `PolicyFile()`
(Get/Raw/Set/SetAndGet/Validate), `DNS()`, `TailnetSettings()`, `Users()`, `Webhooks()`, `Logging()`, `Contacts()`,
`DevicePosture()`, `Tailnets()`. Errors: `IsNotFound(err)`, `ErrorData(err)`.

OAuth clients: created at admin → Trust credentials → OAuth; scopes `devices:core`, `devices:write`, `auth_keys`
(tag required), `dns:read`, `all`. The client secret doubles as an auth key:
`tskey-client-…?ephemeral=true&preauthorized=true` (+ `--advertise-tags=tag:x`); tokens expire after 1 h.

## 9. Tailscale Services (for tier C later)

Named `svc:<name>` with a stable TailVIP + MagicDNS name across hosts; advertised by tagged hosts only, approved by an
admin unless auto-approved in policy; tsnet advertises one via `ListenService` with `ServiceModeHTTP`/`ServiceModeTCP`.
