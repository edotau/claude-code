# Sharing and Publishing

CLI syntax for each feature: [cli.md](cli.md).

## Taildrop (ad-hoc file transfer)

Peer-to-peer between **your own** devices; public alpha, all plans.

- Enable: **Settings > General > Send Files**. **macOS** also needs System Settings > General > Login Items &
  Extensions > Sharing (check Tailscale).
- Send: `tailscale file cp <file> <device>:` (Linux CLI); "Send with Tailscale" right-click on macOS/Windows; Share
  menu on iOS/Android.
- Limits: not to other users' devices (even in the same tailnet) or tagged devices; resume fails when macOS or iOS
  receives.

## Taildrive (persistent folder sharing)

WebDAV-based shares with other users and devices. Alpha, v1.64.0+.

1. Enable with node attributes:

   ```json
   "nodeAttrs": [{ "target": ["autogroup:member"], "attr": ["drive:share", "drive:access"] }]
   ```

2. Grant access (`"access"`: `"rw"` or `"ro"`; list share names instead of `"*"` to restrict):

   ```json
   "grants": [{
     "src": ["group:engineering"],
     "dst": ["tag:nas"],
     "app": { "tailscale.com/cap/drive": [{ "shares": ["*"], "access": "rw" }] }
   }]
   ```

3. Share: `tailscale drive share` on Linux, app GUI on macOS, right-click a folder on Windows.
4. Access at `/<tailnet>/<machine>/<share>` (e.g. `/example.com/nas-device/docs`): network drive on macOS/Windows;
   on Linux mount the WebDAV server at `100.100.100.100:8080`; iOS/Android via the app.

Limits: sharing only from Linux, macOS, Windows and Synology (iOS/Android can access, not share); not usable with
devices shared across tailnets.

## Tailscale Serve (private)

`tailscale serve 3000` publishes `https://<machine-name>.<tailnet>.ts.net` to tailnet members with automatic TLS;
HTTPS must be enabled in the tailnet (the CLI prompts).

- **Identity headers**: `Tailscale-User-Login`, `Tailscale-User-Name`, `Tailscale-User-Profile-Pic` — authenticate on
  them in the backend.
- Respects tailnet access rules; the same port can't serve both Serve and Funnel.
- macOS file/directory serving needs the open-source CLI variant.

## Tailscale Funnel (public)

`tailscale funnel 3000` publishes the same URL publicly; the CLI walks through enabling MagicDNS, HTTPS and the
`funnel` node attribute. Beta, v1.38.3+. DNS resolves to a Funnel relay that TCP-proxies to your device without
decrypting; your device terminates TLS and your IP stays hidden.

- **Ports**: 443, 8443 and 10000 only; TLS only; non-configurable bandwidth limits.
- **macOS**: open-source CLI variant only (not App Store or Standalone).
- **Let's Encrypt rate limits**: frequent certificate requests can trigger a 34-hour wait.

| | Serve | Funnel |
|---|---|---|
| Audience | Tailnet members | Anyone |
| Authentication | Identity headers | None |
| Your IP | Visible to peers | Hidden behind relay |
| Ports | Any | 443, 8443, 10000 |

## Worked examples

| If the user wants to… | Fetch |
|---|---|
| Let teammates preview a laptop web app privately | <https://tailscale.com/docs/use-cases/application-testing/share-local-dev-server-with-team> |
| Put a local dev server on the internet (demo/webhook) | <https://tailscale.com/docs/use-cases/application-testing/share-local-dev-server-with-internet> |
| Host a game server for friends without a public IP | <https://tailscale.com/docs/use-cases/personal-or-at-home-use/share-private-game-server> |
| Run a private Minecraft server | <https://tailscale.com/docs/solutions/set-up-minecraft> |
| Code from an iPad (VS Code, code-server) | <https://tailscale.com/docs/solutions/code-on-ipad-vscode-caddy-code-server> |
| Reach a home/lab inference server (Ollama, LM Studio) | <https://tailscale.com/docs/use-cases/ai-infrastructure-access/connect-inference-servers> |
| Private access to an AI training cluster | <https://tailscale.com/docs/use-cases/ai-infrastructure-access/secure-ai-training-cluster> |
