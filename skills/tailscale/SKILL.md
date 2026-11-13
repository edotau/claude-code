---
name: tailscale
description: >-
  Guide for installing, configuring, and using Tailscale: core mesh VPN (exit
  nodes, subnet routers, access controls and grants, SSH, MagicDNS), Taildrop,
  Tailscale Serve, Funnel, the REST API, Aperture (AI/LLM gateway for API keys,
  cost and usage), and Go applications that embed Tailscale via tsnet. Use when
  someone asks about Tailscale networking, reaching a home or remote machine,
  exposing a local service privately or publicly, sending files between
  devices, LLM API access through Aperture, or writing a Go program that joins
  a tailnet as its own device — even when they describe the scenario without
  naming the product.
---

# Tailscale

Trimmed from [tailscale/tailscale-skill](https://github.com/tailscale/tailscale-skill) @ `4f05d35`
(BSD-3-Clause, `LICENSE`); enterprise, device-management, session-recording, Border0 and containers/Kubernetes
references were dropped — fetch the matching page under `https://tailscale.com/docs/`.

## How references work

`aperture.md`, `api.md`, `tsnet.md` give a stable mental model plus docs URLs to **WebFetch for current detail**;
without
WebFetch, answer inline and name the page. `cli.md`: verify flags with `tailscale help <subcommand>` first.

## Authoring defaults

For a tailnet policy file (deny-by-default):

- **Use grants, not ACLs.** Grants cover network access plus application capabilities (Aperture, Taildrive); ACLs are
  for reading existing policies only. Conversion: [access-control.md](references/access-control.md); comparison:
  <https://tailscale.com/docs/reference/grants-vs-acls>.
- **Grants are only for access rules.** `"ssh"`, `"autoApprovers"`, `"nodeAttrs"` (e.g. Funnel), `"postures"` and
  `"groups"`/`"tagOwners"` keep their own syntax.
- User devices belong to a user account; servers and non-user devices use tags and auth keys.

## Find your task

| I want to… | Read |
|---|---|
| Install on a platform (macOS variants, static binaries, WSL) | [installation.md](references/installation.md) |
| Reach my home machine from anywhere (MagicDNS, SSH, auth keys, key expiry) | [common-tasks.md](references/common-tasks.md) |
| Reach LAN gear that can't run Tailscale, or link two sites | [subnet-routers.md](references/subnet-routers.md) |
| Route all my traffic through home (untrusted Wi-Fi, travel, geo) | [exit-nodes.md](references/exit-nodes.md) |
| Remote desktop, slow/relayed links, DERP, NAT, DNS problems, Tailnet Lock | [connectivity.md](references/connectivity.md) |
| Look up an error or status message | [error-messages.md](references/error-messages.md) |
| Control who can reach which device, or fence off an agent | [access-control.md](references/access-control.md) |
| Send a file or sync a folder (Taildrop, Taildrive); expose a service privately (Serve) or publicly (Funnel) | [sharing-and-publishing.md](references/sharing-and-publishing.md) |
| Reach a home model server (Ollama, LM Studio) from my devices | [installation.md](references/installation.md), then Serve |
| Centralize LLM API keys, see cost/usage, set quotas, govern MCP tools | [aperture.md](references/aperture.md) |
| Build a Go program that joins the tailnet, or give an agent its own identity | [tsnet.md](references/tsnet.md) |
| Script the tailnet (devices, DNS, policy file, webhooks, auth keys) | [api.md](references/api.md) |
| Look up a CLI command or flag | [cli.md](references/cli.md) |
