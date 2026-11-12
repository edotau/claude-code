# Exit Nodes

An exit node routes all non-Tailscale internet traffic through a tailnet device; without one, Tailscale carries only
traffic between tailnet devices. Exit node vs. subnet router: [subnet-routers.md](subnet-routers.md).

## Prerequisites

- Tailscale v1.20+ on both exit node and client devices.
- Exit node must be Linux, macOS, Windows, Android, or tvOS.
- Access policy must permit exit node usage (the default policy does).

## Run an exit node

| Platform | How | Notes |
|---|---|---|
| Linux (recommended) | Enable IP forwarding ([subnet-routers.md](subnet-routers.md#ip-forwarding-linux), incl. `firewalld` masquerade), then `sudo tailscale set --advertise-exit-node` | Kernel routing — best performance |
| macOS | Tailscale menu: **Exit Node > Run Exit Node** | Userspace routing; prevent sleep |
| Windows | System tray: **Exit node > Run exit node** | Userspace routing; enable "Run Unattended" so it persists after logout; prevent sleep |
| Android | App > **Exit Node** > **Run as exit node** | Userspace routing; battery-intensive, keep plugged in |

Then an admin approves it: **Machines** page → device with the **Exit Node** badge → device menu > **Edit route
settings** > enable **Use as exit node**. `autoApprovers` in the policy file skip this step.

## Use an exit node

```bash
sudo tailscale set --exit-node=<exit-node-ip>
sudo tailscale set --exit-node=<exit-node-ip> --exit-node-allow-lan-access=true  # keep LAN access
sudo tailscale set --exit-node=                                                 # stop
```

GUI: macOS menu > **Exit Nodes**; Windows tray > **Use exit node**; iOS/Android app > **Exit Node**.

Using an exit node blocks local network access by default; keep it with `--exit-node-allow-lan-access=true` or the
"Allow Local Network Access" / "Allow LAN access" toggle.

## Notes

- Tailscale can suggest the best exit node by location and latency; MDM system policies can enforce exit node use.
- Enterprise plans can log exit node destinations (**Logs > Network flow logs**); requires log streaming first.
- **GCP Linux VMs**: known issue — refer to the Tailscale docs for the workaround.

## Worked examples

| If the user wants to… | Fetch |
|---|---|
| Test how their app looks or behaves for users in other countries (localization, geo-routing, region-specific content) | <https://tailscale.com/docs/use-cases/application-testing/geo-specific-testing> |
| Protect their browsing on untrusted Wi-Fi by routing traffic through a device they own | <https://tailscale.com/docs/solutions/secure-traffic-public-wifi-appletv> |
