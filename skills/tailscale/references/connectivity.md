# Connectivity: Peer Relay, DERP, and Tailnet Lock

The connection model is stable; peer-relay flags, DERP regions and Tailnet Lock subcommands evolve — **WebFetch the
matching page** below for current names, region IDs and Tailnet Lock setup.

## Mental model

Three paths, tried in order, all WireGuard-encrypted end-to-end (relays can't decrypt); chosen per peer pair:

1. **Direct** — NAT traversal (STUN, port mapping). Preferred.
2. **Peer relay** — user-operated relay device on the tailnet; lower latency than DERP.
3. **DERP relay** — Tailscale's global relay network. Always works.

| Peer A | Peer B | Result |
|---|---|---|
| No NAT | Any | Direct |
| Easy NAT | Easy NAT | Direct |
| Easy NAT | Hard NAT | Relayed |
| Hard NAT | Hard NAT | Relayed |

"Easy NAT" = UPnP / NAT-PMP / PCP, full-cone, consistent port mapping (IPv6 counts as Easy). "Hard NAT" = symmetric
NAT, CGNAT, strict firewalls. DERP also carries connection negotiation: even direct connections exchange DISCO
packets over DERP first.

## Canonical shapes

### Configure a peer relay

On the relay device (Linux/macOS/Windows, not iOS/Android), with the port reachable from clients:

```bash
tailscale set --relay-server-port=<port>
```

Tag the relay devices and grant the capability (it takes no parameters):

```json
"grants": [{
  "src": ["autogroup:member"],
  "dst": ["tag:relay"],
  "app": { "tailscale.com/cap/relay": [] }
}]
```

### Customize the DERP map

Policy file; add custom regions or omit defaults:

```json
"derpMap": {
  "OmitDefaultRegions": false,
  "Regions": {
    "900": {
      "RegionID": 900, "RegionCode": "myderp", "RegionName": "My Custom DERP",
      "Nodes": [{ "Name": "myderp1", "RegionID": 900, "HostName": "derp.example.com" }]
    }
  }
}
```

Official map: `https://controlplane.tailscale.com/derpmap/default`. **Running your own DERP** is generally not
recommended: peer relays fix latency with less complexity and keep sharing and cross-tailnet features.

### Tailnet Lock

Every new node must be signed by a trusted device, so unauthorized nodes can't join even if the coordination server
were compromised.

- **TLK** (Tailnet Lock Key) — Ed25519 key pair on a signing node.
- **TKA** (Tailnet Key Authority) — local signed chain of trusted TLKs and signed node keys.
- **AUM** (Authority Update Message) — signed message that changes trusted-key state.
- **Disablement secrets** — `tailscale lock init` shows ten; any one disables Tailnet Lock and they are the **only**
  way. **Store them safely**; losing them means recovery needs Tailscale support.

```bash
tailscale lock init                                # On a chosen signing node
tailscale lock sign nodekey:<key> tlpub:<key>      # Sign a new device
tailscale lock add tlpub:<key>                     # Add a trusted signing key
tailscale lock remove tlpub:<key>
tailscale lock revoke-keys tlpub:<key>             # Revoke compromised key (needs co-signing)
tailscale lock status                              # TKA state
tailscale lock log                                 # Recent AUMs
tailscale lock disable <secret>                    # Disable with a recovery secret
tailscale lock local-disable                       # Emergency: ignore TL on this node only
```

Constraints:

- Up to 20 signing nodes; rotate TLKs at most once per year.
- **Mutually exclusive with Device Approval.**
- Android can receive signatures but not sign.
- Initial trust is trust-on-first-use from the coordination server — check `tailscale lock status` on several nodes
  after init.

## Remote desktop (RDP, VNC, RustDesk)

Point the client at the target's MagicDNS name or 100.x IP; no port forwarding. Restrict with policy (e.g. a group to
`tcp:3389` on the target tag) and disable key expiry on always-on targets.

- **RDP**: Windows Pro, Enterprise, Education or Server with RDP enabled. Clients: Remote Desktop Connection, Windows
  App (macOS, iOS, Android), Remmina or GNOME Connections (Linux).
- **RustDesk**: no relay/ID server needed. Enable **Direct IP access** under Security (permanent password for
  headless machines), then connect to the Tailscale IP or MagicDNS name.
- **VNC**: server on the target, viewer connects the same way.

## Where to find current information

| Topic | Fetch |
|---|---|
| Connection types taxonomy | <https://tailscale.com/docs/reference/connection-types> |
| Device connectivity overview | <https://tailscale.com/docs/reference/device-connectivity> |
| Traffic routing | <https://tailscale.com/docs/concepts/traffic-routing-through-tailscale> |
| WireGuard background | <https://tailscale.com/docs/concepts/wireguard> |
| Encryption model | <https://tailscale.com/docs/concepts/tailscale-encryption> |
| STUN, port mapping, NAT traversal | <https://tailscale.com/docs/reference/stun-protocol> |
| WireGuard with dynamic IPs | <https://tailscale.com/docs/reference/wireguard-dynamic-ip> |
| DERP servers, regions, custom DERP | <https://tailscale.com/docs/reference/derp-servers> |
| DERP routing troubleshooting | <https://tailscale.com/docs/reference/troubleshooting/network-configuration/derp-routing> |
| Message: no DERP connection | <https://tailscale.com/docs/reference/messages/client/no-derp-connection> |
| Message: no DERP home | <https://tailscale.com/docs/reference/messages/client/no-derp-home> |
| Coordination server down | <https://tailscale.com/docs/reference/coordination-server-down> |
| Message: coordination-server issue | <https://tailscale.com/docs/reference/messages/client/coordination-server-issue> |
| Peer relay (setup, platforms) | <https://tailscale.com/docs/features/peer-relay> |
| Tailnet Lock (setup, concepts) | <https://tailscale.com/docs/features/tailnet-lock> |
| Tailnet Lock whitepaper | <https://tailscale.com/docs/concepts/tailnet-lock-whitepaper> |
| Troubleshooting hub | <https://tailscale.com/docs/reference/troubleshooting> |
| First steps for any network problem | <https://tailscale.com/docs/reference/troubleshooting/basic-network-troubleshooting> |
| Devices can't connect (each other, internet, LAN) | <https://tailscale.com/docs/reference/troubleshooting/connectivity> |
| NAT, routing, DNS, subnet, IP-conflict issues | <https://tailscale.com/docs/reference/troubleshooting/network-configuration> |
| Slow throughput to the internet | <https://tailscale.com/docs/reference/troubleshooting/poor-performance-internet> |
| Slow throughput between tailnet devices | <https://tailscale.com/docs/reference/troubleshooting/poor-performance-tailnet> |
| Can't resolve names (MagicDNS/DNS) | <https://tailscale.com/docs/reference/troubleshooting/resolve-domain-names-failure> |
| macOS, iOS, Apple TV | <https://tailscale.com/docs/reference/troubleshooting/apple> |
| Windows | <https://tailscale.com/docs/reference/troubleshooting/windows> |
| Linux | <https://tailscale.com/docs/reference/troubleshooting/linux> |
| Mobile (battery, app routing) | <https://tailscale.com/docs/reference/troubleshooting/mobile> |
| Cloud (AWS/GCP routes, Oracle, subnets) | <https://tailscale.com/docs/reference/troubleshooting/cloud> |
| Hard-NAT problems | <https://tailscale.com/docs/reference/troubleshooting/network-configuration/hard-nat-issues> |
| CGNAT conflicts (100.64/10) | <https://tailscale.com/docs/reference/troubleshooting/network-configuration/cgnat-conflicts> |
| RDP to a Windows PC without exposing it | <https://tailscale.com/docs/solutions/access-remote-desktops-using-windows-rdp> |
| RustDesk without a relay server | <https://tailscale.com/docs/solutions/access-remote-desktops-with-rustdesk> |
| Home NAS, Plex/JellyFin, file shares from anywhere | <https://tailscale.com/docs/use-cases/personal-or-at-home-use/access-nas-media-file-servers> |
| Ad blocking on all devices (Raspberry Pi) | <https://tailscale.com/docs/solutions/block-ads-all-devices-anywhere-using-raspberry-pi> |
| Home camera from a phone | <https://tailscale.com/docs/solutions/set-up-dogcam> |

For a device that can't run Tailscale, use a subnet router: [subnet-routers.md](subnet-routers.md).

## Answering pattern

- **Slow / relayed**: the NAT table plus `tailscale ping`/`netcheck` ([cli.md](cli.md)) usually suffice; fetch
  connection-types or troubleshooting pages for exact criteria.
- **Peer relay**: the flag + grant above starts it; fetch the page for platform notes.
- **Tailnet Lock**: **always fetch** before enabling for real (admin-console steps, irreversibility if secrets lost).
- **Custom DERP**: recommend against; fetch the DERP-servers page for build steps.
