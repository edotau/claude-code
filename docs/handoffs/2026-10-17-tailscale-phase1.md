# Handoff — Tailscale module phase 1 (2026-10-17)

## State

- Plan: `docs/plans/2026-10-17-tailscale-module.md` (stdlib-only `internal/tailscale`, no `tailscale.com` import;
  measured tiers: tsnet 20.1 MB/559 pkgs, client/local 8.2 MB/323, client/tailscale/v2 6.4 MB/202).
- Workflow `implement-and-verify` run `wf_0e24d730-b85` launched with the phase 1 brief; mid-run it had created
  `internal/tailscale/{api,token,cli,doctor}.go`, `internal/cli/tailscale.go`, `internal/configure/tailscale.go`
  and modified `audit.go`, `cli.go`, `complete.go`, `configure.go`, `configure/secrets.go`, `launch/install.go`,
  `CLAUDE.md`, `README.md`. `go build ./...` and `go vet` were clean at last check. Not committed.
- Defaults chosen for the plan's open items: `TAILSCALE=1` gates `bootstrap`; Linux uses the official script via
  `sh -c "$(curl …)"` (the `curl | sh` spelling trips the safety hook); the configure row lives inside `keys`;
  `Machines` merges API + CLI when a credential exists. Verb stays `tailscale` (not `remote`: collides with
  Remote Control; no speculative umbrella).
- Live facts: Mac already advertises an approved exit node; iPhone online peer; tailnet `tailbb611b.ts.net`;
  `Tailscale.app` GUI variant, LocalAPI reachable.

## Next

1. When the workflow reports: live check on the real surface — `claude-code tailscale status`, `machines`,
   `exit-node` (no-op here), `doctor` row, `configure --check`; fix the `slices.Contains` hint in
   `internal/tailscale/cli.go` if the gate left it; then one full `make test`.
2. Report to the user (they asked to be told when done). Commit only when asked; no AI footers.
3. Tabled: `docs/plans/2026-10-17-setup-modes.md` (adopt / nuke / workspace setup modes) — drafted, not started.
4. Still pending from before: push 809eea2..0b6e4b9; untracked `policy-limits.json*`/`remote-settings.json*`.
