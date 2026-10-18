# Handoff — Tailscale phase 1 + setup modes (2026-10-17/18)

## Shipped (all pushed, origin/main = 1f9b1ce)

- `internal/tailscale` stdlib package + `claude-code tailscale status|machines|connect|key|exit-node`, configure row,
  doctor row, `make tailscale-cli` (`TAILSCALE=1 make bootstrap`). See `README.md` → Tailscale.
- `claude-code setup adopt|nuke|workspace` + `scripts/bootstrap.sh`. See `README.md` → Setup modes
  (the plan parked the agent-driven adopt idea for later).
- `clean --what sessions`; `secrets delete`; `secrets set NAME` prompts on a TTY; redacted confirmation `kJaxH*** (17 chars)`.

## Live state on this Mac

- OAuth client stored (`TAILSCALE_OAUTH_CLIENT_ID`/`_SECRET`, keyring) with scopes `devices:core:read, devices:routes`
  only: `machines` (source=api) and `exit-node --approve` verified live. **`tailscale key` cannot work with it** —
  no `auth_keys` scope and the policy has no `tagOwners`. To enable: add `"tagOwners": {"tag:harness": ["autogroup:admin"]}`,
  generate a new credential with Auth Keys write on `tag:harness`, `secrets set` both names again.
- Personal plan (2026): 6 users, unlimited user devices, 50 tagged resources — the phone/Mac count against nothing.

## Leftovers

- `docs/memory/bank/proj` — seeded by a scratch `setup workspace` live check; gitignored; delete when convenient.
- Chrome tabs (Trust credentials, JSON editor) were left open for the tagOwners step.
- Known: the agent-inflight cap counts a sibling session's agents; the test-repair agent never got a slot this session.
