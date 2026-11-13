---
description: "Live-discover configured providers and append profile-scoped runtime model choices"
argument-hint: "[--profile NAME] [--dry-run] [--format text|json]"
disable-model-invocation: true
allowed-tools: Bash(claude-code models discover *)
---

# /discover-models

Launcher only. Run `claude-code models discover` with the user's `$ARGUMENTS`.
Add `--apply` unless `--dry-run` is present or `--apply` was already supplied. Pass arguments as literal
CLI arguments; never evaluate shell syntax or command substitutions.
Do not run other commands, switch accounts, alter pins/routes, or edit catalogs yourself.

Report each provider/profile's status, source, discovered count, additions, destinations,
and errors/skips from the CLI. A nonzero exit may include independent successful updates;
do not conceal either. Saved entries are append-only selectable history, not proof of
current availability or successful inference. Vendor-owned native catalogs are unchanged.
Stop after the command and report; do not attempt credential repair or inference probes.
