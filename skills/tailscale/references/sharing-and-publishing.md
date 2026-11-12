# Sharing and Publishing

CLI syntax for every feature here: [cli.md](cli.md).

## Taildrop (ad-hoc file transfer)

Peer-to-peer file transfer between **your own** devices. Public alpha; all plans.

- Enable in the admin console: **Settings > General > Send Files**. **macOS** also needs System Settings > General >
  Login Items & Extensions > Sharing (check Tailscale).
- Send: `tailscale file cp <file> <device>:` (Linux CLI); "Send with Tailscale" right-click on macOS/Windows; the
  native Share menu on iOS/Android.
- Limits: not to devices owned by other users (even in the same tailnet), not to tagged devices; transfer resume
  works except when macOS or iOS is the receiver.

## Taildrive (persistent folder sharing)

WebDAV-based persistent shares with other users and devices. Alpha, v1.64.0+.

1. Enable with node attributes:

   ```json
   "nodeAttrs": [
     {
       "target": ["autogroup:member"],
       "attr": ["drive:share", "drive:access"]
     }
   ]
   ```

2. Grant access (`"access"`: `"rw"` or `"ro"`; list share names instead of `"*"` to restrict):

   ```json
   "grants": [
     {
       "src": ["group:engineering"],
       "dst": ["tag:nas"],
       "app": {
         "tailscale.com/cap/drive": [{
           "shares": ["*"],
           "access": "rw"
         }]
       }
     }
   ]
   ```

3. Share: `tailscale drive share` on Linux, the app GUI on macOS, right-click a folder on Windows.
4. Access at `/<tailnet>/<machine>/<share>` (e.g. `/example.com/nas-device/docs`): a network drive on macOS/Windows;
   on Linux mount the WebDAV server at `100.100.100.100:8080`; iOS/Android through the app.

Limits: sharing (server side) only on Linux, macOS, Windows and Synology — iOS/Android can access, not share; not
usable with devices shared across tailnets.

## Tailscale Serve (private)

`tailscale serve 3000` publishes `https://<machine-name>.<tailnet>.ts.net` to tailnet members only, with automatic
TLS. HTTPS must be enabled in the tailnet (the CLI prompts).

- **Identity headers**: Serve adds `Tailscale-User-Login`, `Tailscale-User-Name`, `Tailscale-User-Profile-Pic` — the
  backend can authenticate on them with no extra setup.
- Respects tailnet access rules.
- The same port can't be used for Serve and Funnel simultaneously.
- macOS file/directory serving needs the open-source CLI variant.

## Tailscale Funnel (public)

`tailscale funnel 3000` publishes the same URL to the public internet; the CLI walks through enabling MagicDNS, HTTPS
and the `funnel` node attribute. Beta, v1.38.3+. DNS resolves to a Funnel relay that TCP-proxies to your device
without decrypting it — your device terminates TLS, and your IP stays hidden.

Limitations:

- **Ports**: 443, 8443 and 10000 only.
- **TLS only**; non-configurable bandwidth limits.
- **macOS**: open-source CLI variant only (not App Store or Standalone).
- **Let's Encrypt rate limits**: frequent certificate requests can trigger a 34-hour wait.

| | Serve | Funnel |
|---|---|---|
| **Audience** | Tailnet members only | Anyone on the internet |
| **Authentication** | Tailscale identity headers | None (public) |
| **Your IP** | Visible to tailnet peers | Hidden behind relay |
| **Ports** | Any | 443, 8443, 10000 only |
| **TLS** | Automatic | Automatic |

## Worked examples

| If the user wants to… | Fetch |
|---|---|
| Let teammates preview a web app running on their laptop, privately | <https://tailscale.com/docs/use-cases/application-testing/share-local-dev-server-with-team> |
| Put a local development server on the public internet for a demo or webhook | <https://tailscale.com/docs/use-cases/application-testing/share-local-dev-server-with-internet> |
| Let friends join a game server they host, without a public IP | <https://tailscale.com/docs/use-cases/personal-or-at-home-use/share-private-game-server> |
| Run a private Minecraft server just for their group | <https://tailscale.com/docs/solutions/set-up-minecraft> |
| Code from an iPad against a real development environment (VS Code, code-server) | <https://tailscale.com/docs/solutions/code-on-ipad-vscode-caddy-code-server> |
| Reach a home or lab inference server (Ollama, LM Studio) from other devices | <https://tailscale.com/docs/use-cases/ai-infrastructure-access/connect-inference-servers> |
| Give a team private access to an AI training cluster | <https://tailscale.com/docs/use-cases/ai-infrastructure-access/secure-ai-training-cluster> |
