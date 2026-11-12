# Common Tasks

## MagicDNS

- On by default for tailnets created after October 20, 2022; otherwise enable it in the admin console under **DNS**.
- FQDN: `<machine-name>.<tailnet-name>.ts.net` (e.g. `monitoring.yak-bebop.ts.net`); short names work inside the
  tailnet via the automatic search domain.
- Some macOS tools (`host`, `nslookup`) bypass system DNS and won't resolve MagicDNS names — use `ping` or `dig`.
- Devices shared from other tailnets must be reached by their full domain name.

## Tailscale SSH

On the server: `tailscale set --ssh`; from any tailnet device: `ssh user@machine-name`. Existing SSH config and keys
are untouched; non-Tailscale SSH keeps working.

- **Server platforms**: Linux fully; macOS only with the open-source `tailscaled` CLI variant (not App Store or
  standalone GUI). Clients can connect from any platform.
- **Access**: the policy file's `"ssh"` section, incl. check mode for root — [access-control.md](access-control.md).
- **Recording**: SSH sessions can be recorded for audit; configure in the admin console.

## Auth keys

For headless provisioning (CI runners, servers, containers): generate in the admin console under **Settings > Keys**,
then `sudo tailscale up --auth-key=tskey-auth-xxxxx`. Keys can be reusable or single-use, **ephemeral** (device
removed when it goes offline), **pre-approved** (no admin approval) and **tagged** (tag identity instead of user).
Creating keys via the API: [api.md](api.md).

## Key expiry

Device keys expire after 180 days by default and the device must re-authenticate. Disable per device from the
Machines page > device menu > **Disable key expiry**; tagged devices have expiry disabled by default. For long-running
servers, disable expiry or use tagged auth keys.
