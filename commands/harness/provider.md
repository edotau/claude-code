---
description: "Switch Claude Code's session provider, or show the current pin and credential state"
argument-hint: "[<provider-name> | --status]"
allowed-tools: "Bash(claude-code switch:*), Bash(claude-code profile:*), Bash(claude-code models:*), Read"
---

# /harness:provider — Switch LLM Provider

Flip Claude Code between the registered providers (the vault's provider overlay, edited with
`claude-code vault edit`, overlaying `internal/providers/defaults.json` by name). The command pins
the session provider via `claude-code switch` (alias: `use`), which writes the vault pins.

> **Relaunch to apply.** `claude-code switch <provider>` only rewrites the pin; the running
> session keeps its current provider and auth intact. The new provider takes effect on the
> next launch. After it runs, **exit and relaunch** (`claude`), or start a one-off launch
> on a different provider with `claude --provider <p>` without touching the durable pin.

## Task

$ARGUMENTS

## What to do

Dispatch on the argument:

- **`--status`, empty, or `status`** — run:
  ```bash
  claude-code profile
  claude-code models
  ```
  Report the default/pinned provider, resolved tier→model slots, credential state, and
  whether the loopback router is needed. Change nothing.

- **A provider name** (e.g. `anthropic`, `gemini`, any name `claude-code profile` lists) — run:
  ```bash
  claude-code switch <name>
  ```
  This pins the session provider in the vault. The next launch regenerates the
  routing env for that provider; credentials resolve through its `auth` ladder
  (`README.md` → Credentials).

- **Anything else** — run `claude-code profile` first to list the valid set, then reject
  with that list.

After a switch, surface the command's output verbatim (old → new) and **remind the user to
restart Claude Code** — the running session keeps its old settings until relaunched.

## Notes

- The provider overlay replaces the built-ins **by name**; `"name": null` removes a built-in,
  and `default`/`fallback` override when set.
- A one-launch override never touches the durable pin: `claude --provider <p> --model <m>`.
- Selection order (provider): `--provider` → `HARNESS_PROVIDER` → registry `default`; process
  env always beats the vault pins.

## See also

- `claude-code profile <name>` — routes, auth, models for one provider
- `claude-code doctor` — checks binaries, providers, credentials, and router health
- `README.md` → Providers / Credentials / Routing
