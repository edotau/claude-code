# Connectivity: Peer Relay, DERP, and Tailnet Lock

The connection model is stable; peer-relay flags, DERP regions and Tailnet Lock subcommands evolve — **WebFetch the
matching page** below for current flag names, region IDs and step-by-step Tailnet Lock setup.

## Mental model

Three paths, tried in order, all WireGuard-encrypted end-to-end; relays (peer or DERP) cannot decrypt traffic, and
the path is chosen per peer pair, not tailnet-wide:

1. **Direct peer-to-peer** — NAT traversal (STUN, port mapping). Preferred.
2. **Peer relay** — a user-operated relay device on the tailnet; lower latency than DERP.
3. **DERP relay** — Tailscale's global relay network. Always works.

| Peer A | Peer B | Result |
|---|---|---|
| No NAT | Any NAT type | Direct |
| Easy NAT | Easy NAT | Direct |
| Easy NAT | Hard NAT | Relayed (peer relay or DERP) |
| Hard NAT | Hard NAT | Relayed (peer relay or DERP) |

"Easy NAT" = UPnP / NAT-PMP / PCP support, full-cone NAT, consistent port mapping (IPv6 counts as Easy). "Hard NAT" =
symmetric NAT, CGNAT, or strict firewalls.

DERP also handles **connection negotiation**: even direct connections exchange discovery (DISCO) packets over DERP
before switching to direct.

## Canonical shapes

### Configure a peer relay

On the relay device (Linux/macOS/Windows — not iOS/Android), with the port reachable from the devices that will use
it (public IP or port-forwarded):

```bash
tailscale set --relay-server-port=<port>
```

Then tag the relay devices and grant the relay capability:

```json
"grants": [{
  "src": ["autogroup:member"],
  "dst": ["tag:relay"],
  "app": {
    "tailscale.com/cap/relay": [] // the relay capability takes no parameters
  }
}]
```

### Customize the DERP map

In the tailnet policy file, add custom DERP regions or omit defaults:

```json
"derpMap": {
  "OmitDefaultRegions": false,
  "Regions": {
    "900": {
      "RegionID": 900,
      "RegionCode": "myderp",
      "RegionName": "My Custom DERP",
      "Nodes": [{
        "Name": "myderp1",
        "RegionID": 900,
        "HostName": "derp.example.com"
      }]
    }
  }
}
```

Official map with current region IDs: `https://controlplane.tailscale.com/derpmap/default`. **Running your own DERP**
is generally not recommended: peer relays solve the latency problem with less complexity and keep device sharing and
cross-tailnet features.

### Tailnet Lock

Every new node must be signed by an existing trusted device, so unauthorized nodes can't join even if the
coordination server were compromised.

- **Tailnet Lock Key (TLK)** — Ed25519 key pair on a signing node; admins designate which are trusted.
- **Tailnet Key Authority (TKA)** — local signed chain (think git) tracking trusted TLKs and signed node keys.
- **Authority Update Message (AUM)** — signed message that modifies trusted-key state.
- **Disablement secrets** — `tailscale lock init` displays ten; any one disables Tailnet Lock, and they are the
  **only** way to. **Store them in a safe / password manager**; losing them means the tailnet cannot be recovered
  without Tailscale support.

Core CLI flow (full setup is admin-console-driven):

```bash
tailscale lock init                                # On a chosen signing node
tailscale lock sign nodekey:<key> tlpub:<key>      # Sign a new device's join
tailscale lock add tlpub:<key>                     # Add a trusted signing key
tailscale lock remove tlpub:<key>                  # Remove one
tailscale lock revoke-keys tlpub:<key>             # Revoke compromised key (needs co-signing)
tailscale lock status                              # Inspect TKA state
tailscale lock log                                 # Recent AUMs
tailscale lock disable <secret>                    # Disable using a recovery secret
tailscale lock local-disable                       # Emergency: ignore TL on this node only
```

Constraints:

- Up to 20 signing nodes.
- Rotate TLKs at most once per year (TKA growth bound).
- **Mutually exclusive with Device Approval** — pick one.
- Android devices can receive signatures but cannot sign.
- Initial trust is "trust on first use" from the coordination server — verify `tailscale lock status` on multiple
  nodes after init.

## Remote desktop over the tailnet (RDP, VNC, RustDesk)

Point the desktop client at the target's MagicDNS name or 100.x IP — no port forwarding, nothing exposed publicly.
Restrict who can reach it with policy (for example only a group to `tcp:3389` on the target tag), and disable key
expiry on always-on targets.

- **RDP**: the Windows PC needs Pro, Enterprise, Education or Server edition with RDP enabled. Clients: Remote
  Desktop Connection, the Windows App (macOS, iOS, Android), Remmina or GNOME Connections (Linux).
- **RustDesk**: no relay/ID server needed over Tailscale. Enable **Direct IP access** under Security (set a permanent
  password for headless machines), then connect to the Tailscale IP or MagicDNS name.
- **VNC**: run the server on the target and connect the viewer the same way.

## Where to find current information

### Connection types & how Tailscale connects

| User is asking about… | Fetch |
|---|---|
| Direct vs relayed connection — full taxonomy | <https://tailscale.com/docs/reference/connection-types> |
| Device connectivity overview | <https://tailscale.com/docs/reference/device-connectivity> |
| How traffic routes through Tailscale | <https://tailscale.com/docs/concepts/traffic-routing-through-tailscale> |
| WireGuard background | <https://tailscale.com/docs/concepts/wireguard> |
| Encryption model | <https://tailscale.com/docs/concepts/tailscale-encryption> |
| STUN, port mapping, NAT traversal mechanics | <https://tailscale.com/docs/reference/stun-protocol> |
| WireGuard with dynamic IPs | <https://tailscale.com/docs/reference/wireguard-dynamic-ip> |

### DERP

| Topic | Fetch |
|---|---|
| DERP servers — purpose, regions, custom DERP | <https://tailscale.com/docs/reference/derp-servers> |
| Troubleshooting DERP routing | <https://tailscale.com/docs/reference/troubleshooting/network-configuration/derp-routing> |
| Client message: no DERP connection | <https://tailscale.com/docs/reference/messages/client/no-derp-connection> |
| Client message: no DERP home | <https://tailscale.com/docs/reference/messages/client/no-derp-home> |
| Coordination server down | <https://tailscale.com/docs/reference/coordination-server-down> |
| Coordination-server-issue client message | <https://tailscale.com/docs/reference/messages/client/coordination-server-issue> |

### Peer relay

| Topic | Fetch |
|---|---|
| Peer relay overview, setup, platform support | <https://tailscale.com/docs/features/peer-relay> |

### Tailnet Lock

| Topic | Fetch |
|---|---|
| Tailnet Lock — full setup + concepts | <https://tailscale.com/docs/features/tailnet-lock> |
| Whitepaper (cryptographic design) | <https://tailscale.com/docs/concepts/tailnet-lock-whitepaper> |

### Connectivity troubleshooting

| If the user is troubleshooting… | Fetch |
|---|---|
| Anything, not sure where to start (troubleshooting hub) | <https://tailscale.com/docs/reference/troubleshooting> |
| First steps for any network problem | <https://tailscale.com/docs/reference/troubleshooting/basic-network-troubleshooting> |
| Devices can't connect to each other, the internet, or the LAN | <https://tailscale.com/docs/reference/troubleshooting/connectivity> |
| NAT, routing, DNS, subnet, or IP-conflict issues | <https://tailscale.com/docs/reference/troubleshooting/network-configuration> |
| Slow throughput to internet destinations | <https://tailscale.com/docs/reference/troubleshooting/poor-performance-internet> |
| Slow throughput between tailnet devices | <https://tailscale.com/docs/reference/troubleshooting/poor-performance-tailnet> |
| Can't resolve domain names (MagicDNS/DNS) | <https://tailscale.com/docs/reference/troubleshooting/resolve-domain-names-failure> |
| A macOS, iOS, or Apple TV problem | <https://tailscale.com/docs/reference/troubleshooting/apple> |
| A Windows problem | <https://tailscale.com/docs/reference/troubleshooting/windows> |
| A Linux problem | <https://tailscale.com/docs/reference/troubleshooting/linux> |
| A mobile (battery, app routing) problem | <https://tailscale.com/docs/reference/troubleshooting/mobile> |
| A cloud environment problem (AWS/GCP routes, Oracle, subnets) | <https://tailscale.com/docs/reference/troubleshooting/cloud> |
| A specific hard-NAT problem | <https://tailscale.com/docs/reference/troubleshooting/network-configuration/hard-nat-issues> |
| CGNAT conflicts (with 100.64/10 ranges) | <https://tailscale.com/docs/reference/troubleshooting/network-configuration/cgnat-conflicts> |

### Remote desktop

| If the user wants to… | Fetch |
|---|---|
| Remote into a Windows PC (RDP) from elsewhere without exposing it to the internet | <https://tailscale.com/docs/solutions/access-remote-desktops-using-windows-rdp> |
| Use RustDesk to reach another desktop, without running or paying for a relay server | <https://tailscale.com/docs/solutions/access-remote-desktops-with-rustdesk> |

### At-home access (client on each device, reach by MagicDNS or 100.x)

For a device that can't run Tailscale, use a subnet router instead: [subnet-routers.md](subnet-routers.md).

| If the user wants to… | Fetch |
|---|---|
| Reach their home NAS, Plex/JellyFin, or file shares from anywhere | <https://tailscale.com/docs/use-cases/personal-or-at-home-use/access-nas-media-file-servers> |
| Block ads across all their devices, even when away from home | <https://tailscale.com/docs/solutions/block-ads-all-devices-anywhere-using-raspberry-pi> |
| Check a home camera from their phone while out | <https://tailscale.com/docs/solutions/set-up-dogcam> |

## Answering pattern

- **Slow / relayed**: the mental model, NAT table and `tailscale ping`/`netcheck` output ([cli.md](cli.md)) usually
  suffice; fetch connection-types or troubleshooting pages for exact criteria.
- **Peer relay**: the flag + grant above is enough to start; fetch the peer-relay page for platform notes.
- **Tailnet Lock**: **always fetch** before the user enables it for real — admin-console steps and irreversibility
  risk (lost disablement secrets).
- **Custom DERP**: recommend against by default; fetch the DERP-servers page for build/operate steps.
