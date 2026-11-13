# Common Tasks

## MagicDNS

- On by default for tailnets created after October 20, 2022; otherwise enable under **DNS** in the admin console.
- FQDN `<machine-name>.<tailnet-name>.ts.net` (e.g. `monitoring.yak-bebop.ts.net`); short names work via the
  automatic search domain. Devices shared from other tailnets need the full domain name.
- Some macOS tools (`host`, `nslookup`) bypass system DNS and won't resolve MagicDNS names — use `ping` or `dig`.

## Tailscale SSH

Server: `tailscale set --ssh`; client: `ssh user@machine-name`. Existing SSH config and keys are untouched.

- **Server platforms**: Linux; macOS only with the open-source `tailscaled` CLI variant (not App Store/standalone
  GUI). Any platform can connect.
- **Access**: policy file `"ssh"` section, incl. check mode — [access-control.md](access-control.md).
- **Recording**: configure in the admin console.

## Auth keys

Headless provisioning: generate under **Settings > Keys**, then `sudo tailscale up --auth-key=tskey-auth-xxxxx`.
Keys can be reusable or single-use, **ephemeral** (device removed when offline), **pre-approved** and **tagged**.
Via the API: [api.md](api.md).

## Key expiry

Device keys expire after 180 days and the device must re-authenticate. Disable per device: Machines page > device
menu > **Disable key expiry**; tagged devices have expiry disabled by default. Long-running servers: disable expiry
or use tagged auth keys.
