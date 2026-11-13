# Exit Nodes

An exit node routes all non-Tailscale internet traffic through a tailnet device. Vs. subnet routers:
[subnet-routers.md](subnet-routers.md).

## Prerequisites

- Tailscale v1.20+ on exit node and client.
- Exit node on Linux, macOS, Windows, Android, or tvOS; the access policy must permit it (default does).

## Run an exit node

| Platform | How | Notes |
|---|---|---|
| Linux (recommended) | Enable IP forwarding ([subnet-routers.md](subnet-routers.md#ip-forwarding-linux), incl. `firewalld` masquerade), then `sudo tailscale set --advertise-exit-node` | Kernel routing — best performance |
| macOS | Menu: **Exit Node > Run Exit Node** | Userspace routing; prevent sleep |
| Windows | Tray: **Exit node > Run exit node** | Userspace; enable "Run Unattended"; prevent sleep |
| Android | App > **Exit Node** > **Run as exit node** | Userspace; battery-intensive, keep plugged in |

Then an admin approves it: **Machines** → device with **Exit Node** badge → menu > **Edit route settings** > **Use as
exit node**. `autoApprovers` skip this.

## Use an exit node

```bash
sudo tailscale set --exit-node=<exit-node-ip>
sudo tailscale set --exit-node=<exit-node-ip> --exit-node-allow-lan-access=true  # keep LAN access
sudo tailscale set --exit-node=                                                 # stop
```

GUI: macOS menu > **Exit Nodes**; Windows tray > **Use exit node**; iOS/Android app > **Exit Node**. LAN access is
blocked by default; keep it with `--exit-node-allow-lan-access=true` or the "Allow Local Network Access" toggle.

## Notes

- Tailscale can suggest the best exit node by location and latency; MDM can enforce exit node use.
- Enterprise plans log exit node destinations (**Logs > Network flow logs**; needs log streaming first).
- **GCP Linux VMs**: known issue — see the Tailscale docs for the workaround.

## Worked examples

| If the user wants to… | Fetch |
|---|---|
| Test app behavior for users in other countries | <https://tailscale.com/docs/use-cases/application-testing/geo-specific-testing> |
| Protect browsing on untrusted Wi-Fi via their own device | <https://tailscale.com/docs/solutions/secure-traffic-public-wifi-appletv> |
