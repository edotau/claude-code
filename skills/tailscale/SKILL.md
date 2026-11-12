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

Trimmed from [tailscale/tailscale-skill](https://github.com/tailscale/tailscale-skill) @ `4f05d35`
(BSD-3-Clause, `LICENSE`); the enterprise, device-management, session-recording, Border0 and containers/Kubernetes
references were dropped.
For those topics, fetch the matching page under `https://tailscale.com/docs/`.

Tailscale is a WireGuard mesh VPN; a **tailnet** is your private network of authenticated devices and users.

## How references work

**Descriptive references** (`aperture.md`, `api.md`, `tsnet.md`) hold a stable mental model plus canonical
`tailscale.com/docs/...` URLs to **WebFetch for current detail** — prefer the live page for specifics (config keys,
env vars, models, pricing). Without WebFetch, answer from the inline content and name the doc page to consult.

**Operational reference** (`cli.md`) is for running commands on the user's machine. Verify a flag locally with
`tailscale help <subcommand>` before suggesting it; the docs URL is the second fallback.

The rest are self-contained — read the file, answer.

## Authoring defaults

When writing or editing a tailnet policy file (deny-by-default):

- **Use grants, not ACLs.** Grants cover network-layer access plus application capabilities (Aperture, Taildrive) in
  one form; ACLs are for reading existing policies only. Conversion pattern:
  [access-control.md](references/access-control.md); comparison: <https://tailscale.com/docs/reference/grants-vs-acls>.
- **Grants are only for access rules.** `"ssh"`, `"autoApprovers"` (routes, exit nodes), `"nodeAttrs"` (e.g. Funnel),
  `"postures"` and `"groups"`/`"tagOwners"` keep their own syntax.
- Tie user devices to a user account; use tags and auth keys for servers and non-user devices.

## Find your task

| I want to… | Read |
|---|---|
| Install on a platform (macOS variants, static binaries, WSL) | [installation.md](references/installation.md) |
| Reach my home machine, files or media from anywhere (MagicDNS, SSH, auth keys, key expiry) | [common-tasks.md](references/common-tasks.md) |
| Reach LAN gear that can't run Tailscale (router, NAS, smart home), or link two sites | [subnet-routers.md](references/subnet-routers.md) |
| Route all my traffic through home (untrusted Wi-Fi, travel, geo) | [exit-nodes.md](references/exit-nodes.md) |
| Remote desktop, slow/relayed links, DERP, NAT, DNS problems, Tailnet Lock | [connectivity.md](references/connectivity.md) |
| Look up a specific error or status message | [error-messages.md](references/error-messages.md) |
| Control who/what can reach which device, or fence off an agent | [access-control.md](references/access-control.md) |
| Send a file to another device, or sync a folder | [sharing-and-publishing.md](references/sharing-and-publishing.md) (Taildrop, Taildrive) |
| Expose a local API or dev server to my tailnet only | [sharing-and-publishing.md](references/sharing-and-publishing.md) (Serve) |
| Put a service on the public internet | [sharing-and-publishing.md](references/sharing-and-publishing.md) (Funnel) |
| Reach a home model server (Ollama, LM Studio) from my other devices | [installation.md](references/installation.md), then Serve |
| Centralize LLM API keys, see cost/usage, set quotas, govern MCP tools | [aperture.md](references/aperture.md) |
| Build a Go program that joins the tailnet as its own device, or give an agent its own identity | [tsnet.md](references/tsnet.md) |
| Script the tailnet (devices, DNS, policy file, webhooks, auth keys) | [api.md](references/api.md) |
| Look up a CLI command or flag | [cli.md](references/cli.md) |
