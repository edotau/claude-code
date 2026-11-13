# Subnet Routers

A subnet router is a tailnet device that gateways to networks whose endpoints don't run Tailscale (printers, IoT,
RDS/Cloud SQL, whole VPCs). Devices behind it don't count toward the plan's device limit.

## Setup

### IP forwarding (Linux)

Shared with exit nodes; macOS forwards automatically when advertising routes.

```bash
echo 'net.ipv4.ip_forward = 1' | sudo tee -a /etc/sysctl.d/99-tailscale.conf
echo 'net.ipv6.conf.all.forwarding = 1' | sudo tee -a /etc/sysctl.d/99-tailscale.conf
sudo sysctl -p /etc/sysctl.d/99-tailscale.conf
firewall-cmd --permanent --add-masquerade   # only if using firewalld
```

### Advertise, approve, allow, accept

1. On a device on the target network: `sudo tailscale set --advertise-routes=192.168.1.0/24,10.0.0.0/16` (no `sudo`
   in Windows PowerShell). IPv4 and IPv6 both work, except Apple TV (IPv4 only). Android: app > Settings > Subnet
   routing > Add route.
2. Approve: **Machines** → device with **Subnets** badge → **Subnets** > **Edit** > approve > **Save**; skipped when
   `autoApprovers` cover the route.
3. If the default policy was modified, grant access:

   ```json
   "grants": [{ "src": ["group:dev"], "dst": ["192.168.1.0/24"], "ip": ["*:*"] }]
   ```

4. **Linux clients** opt in with `sudo tailscale set --accept-routes`; macOS, Windows, iOS and Android accept
   automatically.
5. Verify: ping the router's Tailscale IP (`tailscale ip -4` on it), then a device behind it.

## Advanced

- **High availability**: two routers advertising the same routes fail over automatically (see Tailscale's HA docs).
- **Overlapping routes**: longest prefix match (Router A `10.0.0.0/16`, B `10.0.0.0/24`: `10.0.0.5` → B, `10.0.1.5`
  → A). **No fallback to a less-specific route when the more-specific router goes offline** — have all routers
  advertise both the broad and specific prefixes.
- **Disable SNAT** (Linux only; default shows the router's IP as source): `tailscale up --snat-subnet-routes=false`;
  devices behind the subnet then need return routes for `100.64.0.0/10` via the router.
- **DNS for subnet resources**: configure split DNS on the admin console **DNS** page.

## Worked examples

| If the user wants to… | Fetch |
|---|---|
| Reach a printer, camera, IoT device or home/office subnet that can't run Tailscale | <https://tailscale.com/docs/use-cases/personal-or-at-home-use/access-devices-without-tailscale> |
