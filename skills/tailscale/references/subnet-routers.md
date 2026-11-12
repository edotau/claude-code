# Subnet Routers

A subnet router is a tailnet device that gateways to networks whose endpoints don't run Tailscale (printers, IoT,
RDS/Cloud SQL, whole VPCs). Devices behind a subnet router don't count toward the plan's device limit.

## Setup

### IP forwarding (Linux)

Shared with exit nodes ([exit-nodes.md](exit-nodes.md)); macOS handles forwarding automatically when advertising
routes.

```bash
echo 'net.ipv4.ip_forward = 1' | sudo tee -a /etc/sysctl.d/99-tailscale.conf
echo 'net.ipv6.conf.all.forwarding = 1' | sudo tee -a /etc/sysctl.d/99-tailscale.conf
sudo sysctl -p /etc/sysctl.d/99-tailscale.conf
firewall-cmd --permanent --add-masquerade   # only if using firewalld
```

### Advertise, approve, allow, accept

1. On a device on the target network: `sudo tailscale set --advertise-routes=192.168.1.0/24,10.0.0.0/16` (no `sudo`
   in Windows PowerShell). IPv4 and IPv6 subnets both work, except Apple TV (IPv4 only). Android: app > Settings >
   Subnet routing > Add route.
2. Approve: **Machines** page → device with the **Subnets** badge → **Subnets** > **Edit** > approve > **Save**.
   Skipped when `autoApprovers` cover the route.
3. If the default policy was modified, grant access to the subnet:

   ```json
   "grants": [
     {
       "src": ["group:dev"],
       "dst": ["192.168.1.0/24"],
       "ip": ["*:*"]
     }
   ]
   ```

4. **Linux clients** must opt in with `sudo tailscale set --accept-routes`; macOS, Windows, iOS and Android accept
   routes automatically.
5. Verify: from a client, ping the router's Tailscale IP (`tailscale ip -4` on the router), then a device behind it.

## Advanced

### High availability

Two subnet routers advertising the same routes fail over automatically; refer to Tailscale's high availability docs.

### Overlapping routes

Routers may advertise overlapping prefixes; Tailscale uses longest prefix match. With Router A on `10.0.0.0/16` and
Router B on `10.0.0.0/24`, `10.0.0.5` goes via B and `10.0.1.5` via A.

**Tailscale does not fall back to a less-specific route if the more-specific router goes offline.** Have all routers
advertise both the broad and specific prefixes.

### Disable SNAT

By default tailnet traffic appears to come from the subnet router's IP. To preserve source IPs (Linux only):

```bash
tailscale up --snat-subnet-routes=false
```

Devices behind the subnet then need return routes sending `100.64.0.0/10` back through the subnet router.

### DNS for subnet resources

Configure split DNS in the admin console (**DNS** page) to resolve names via an internal DNS server on the subnet.

## Subnet routers vs. exit nodes

| | Subnet router | Exit node |
|---|---|---|
| **Purpose** | Access specific private subnets | Route all internet traffic |
| **Traffic** | Only traffic to advertised CIDRs | All non-Tailscale traffic |
| **Use case** | Reach printers, databases, VPCs | Secure public browsing, geo-access |

## Worked examples

| If the user wants to… | Fetch |
|---|---|
| Reach a printer, camera, IoT device, or whole home/office subnet that can't run Tailscale itself | <https://tailscale.com/docs/use-cases/personal-or-at-home-use/access-devices-without-tailscale> |
