---
name: tailscale
description: >-
  Guide for installing, configuring, and using Tailscale: core mesh VPN (exit
  nodes, subnet routers, access controls and grants, SSH, MagicDNS), Taildrop,
  Tailscale Serve, Funnel, the REST API, Aperture (AI/LLM gateway for API keys,
  cost and usage), and building Go applications that embed Tailscale via the
  tsnet library. Use when someone asks about Tailscale networking, reaching a
  home or remote machine, exposing a local service privately or publicly,
  sending files between devices, LLM API access through Aperture, or writing a
  Go program that joins a tailnet as its own device — even when they describe
  the scenario without naming the product.
---

# Tailscale

Trimmed from [tailscale/tailscale-skill](https://github.com/tailscale/tailscale-skill) @ `4f05d35` (BSD-3-Clause, `LICENSE`);
the enterprise, device-management, session-recording, Border0 and containers/Kubernetes references were dropped.
For those topics, fetch the matching page under `https://tailscale.com/docs/`.

Tailscale is a zero-config mesh VPN that builds a private WireGuard network (a **tailnet**) between your devices,
connecting them peer to peer rather than through a central gateway.

## How references work

**Descriptive references** (`aperture.md`, `api.md`, `tsnet.md`) help you *describe* a topic: explain it, draft
configuration, recommend an approach. They hold a stable mental model inline plus canonical `tailscale.com/docs/...`
URLs to **WebFetch for current detail** — prefer the live page for specifics (config keys, env vars, models, pricing).
Without WebFetch, answer from the inline content and name the doc page to consult.

**Operational reference** (`cli.md`) is for running commands on the user's machine. Verify a flag locally with
`tailscale help <subcommand>` before suggesting it; the docs URL is the second fallback.

The rest (`access-control.md`, `common-tasks.md`, `connectivity.md`, `error-messages.md`, `exit-nodes.md`,
`installation.md`, `sharing-and-publishing.md`, `subnet-routers.md`) are self-contained — read the file, answer.

## Core concepts

- **Tailnet**: your private network of authenticated devices and users.
- **MagicDNS**: automatic DNS names for every device (`ssh my-server`).
- **100.x.y.z addresses**: each device gets a stable Tailscale IP in the CGNAT range.
- **Tailnet policy file**: JSON in the admin console defining access, groups, tags and SSH rules. Deny-by-default.

## Authoring defaults

When writing or editing a tailnet policy file:

- **Use grants, not ACLs.** Grants cover network-layer access plus application capabilities (Aperture, Taildrive) in
  one form; ACLs are for reading existing policies only. Conversion pattern: `references/access-control.md`;
  comparison: <https://tailscale.com/docs/reference/grants-vs-acls>.
- **Grants are only for access rules.** `"ssh"`, `"autoApprovers"` (routes, exit nodes), `"nodeAttrs"` (e.g. Funnel),
  `"postures"` and `"groups"`/`"tagOwners"` keep their own syntax.

## Quick start

macOS/Windows: <https://tailscale.com/download>. Mobile: the platform's app store. Linux and WSL:

```bash
curl -fsSL https://tailscale.com/install.sh | sh   # Install (Linux)
sudo tailscale up                                    # Connect
tailscale status                                     # Verify
```

Tie user devices to a user account; use tags and auth keys for servers and non-user devices.

## Find your task

| I want to… | Read |
|---|---|
| Reach my home machine, files or media from anywhere | `references/installation.md`, then `common-tasks.md` (MagicDNS, SSH) |
| Reach LAN gear that can't run Tailscale (router, NAS, smart home), or link two sites | `references/subnet-routers.md` |
| Route all my traffic through home (untrusted Wi-Fi, travel, geo) | `references/exit-nodes.md` |
| Remote desktop, slow/relayed links, DERP, NAT, DNS problems | `references/connectivity.md` |
| Look up a specific error or status message | `references/error-messages.md` |
| Control who/what can reach which device, or fence off an agent | `references/access-control.md` |
| Send a file to another device, or sync a folder | `references/sharing-and-publishing.md` (Taildrop, Taildrive) |
| Expose a local API or dev server to my tailnet only | `references/sharing-and-publishing.md` (Serve) |
| Put a service on the public internet | `references/sharing-and-publishing.md` (Funnel) |
| Reach a home model server (Ollama, LM Studio) from my other devices | `references/installation.md`, then Serve |
| Centralize LLM API keys, see cost/usage, set quotas, govern MCP tools | `references/aperture.md` |
| Build a Go program that joins the tailnet as its own device, or give an agent its own identity | `references/tsnet.md` |
| Script the tailnet (devices, DNS, policy file, webhooks, auth keys) | `references/api.md` |
| Look up a CLI command or flag | `references/cli.md` |

## CLI quick reference

| Command | What it does |
|---------|-------------|
| `tailscale up` | Connect to your tailnet |
| `tailscale down` | Disconnect |
| `tailscale status` | Show connected devices |
| `tailscale ip` | Show your Tailscale IP addresses |
| `tailscale ping <host>` | Test connectivity to a device |
| `tailscale set --ssh` | Enable Tailscale SSH on this device |
| `tailscale set --advertise-exit-node` | Advertise as an exit node |
| `tailscale set --exit-node=<ip>` | Use a specific exit node |
| `tailscale set --advertise-routes=<cidr>` | Advertise subnet routes |
| `tailscale set --accept-routes` | Accept advertised subnet routes (Linux) |
