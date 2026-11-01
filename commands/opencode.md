---
description: >-
  OpenCode via `claude-code opencode <verb>`: a bare task runs `claude-code ask --agent opencode --tools`
  (routed); `ask|resume|review|explore|transfer|status|tail|result|cancel|setup|catalog|profile|on|off` pass straight through.
argument-hint: "[profile=<p>] [model=<m>] [<verb>] <task>  (bare = routed ask)"
allowed-tools: Bash
---

# /opencode — OpenCode via `claude-code opencode <verb>`

Launcher only. Modes, verbs and the report contract live in `agents/opencode.md`;
`claude-code opencode -h` lists the surface.

`profile=<p>` / `model=<m>` anywhere in `$ARGUMENTS` (strip them first) become `--profile <p>` /
`--model <m>` on the `claude-code opencode <verb>` call or the routed `ask` (not on `profile`/`on`/`off`); a verb that
lacks the flag errors, so relay that. `<p>` is a vault profile (`dev`, `qa`, `prod`, …), `<m>` an alias or wire id.

Parse the first token of `$ARGUMENTS`:

- `ask|resume|review|explore|transfer|status|tail|result|cancel|setup|catalog` — run
  `claude-code opencode <verb> <rest>`
  straight through.
- `profile [<profile>|--clear]` — run `claude-code opencode profile <rest>`: bare shows opencode's profile
  pin and on/off state; a profile pins opencode there (relaunch to apply).
- `on|off` — run `claude-code opencode on|off`: route to opencode again, or stop routing to it.
- anything else — run `claude-code ask --agent opencode --tools "<task>"` (routed; the default model
  is in `internal/router/table.json`).

Relay the output. `--background` → relay the `job <id>` line and name
`claude-code opencode status|result|cancel`. Replays: `claude-code ask --log` / `ask --retry <id>`
still work.
