---
description: >-
  OpenCode via `claude-code opencode <verb>`: a bare task runs `claude-code ask --agent opencode --tools`
  (kimi-k3.1-pro high, routed); `ask|resume|review|status|result|cancel|setup|models|env` pass straight through.
argument-hint: "[ask|resume|review|status|result|cancel|setup|models|env] [args…] | <task>"
allowed-tools: Bash
---

# /opencode — OpenCode via `claude-code opencode <verb>`

Launcher only. Modes, verbs and the report contract live in `agents/opencode.md`;
`claude-code opencode -h` lists the surface.

Parse the first token of `$ARGUMENTS`:

- `ask|resume|review|status|result|cancel|setup|models` — run `claude-code opencode <verb> <rest>`
  straight through.
- `env [dev|qa|prod]` — run `claude-code opencode env <rest>`: bare shows opencode's account; an env moves
  only opencode's profile row there (relaunch to apply).
- anything else — run `claude-code ask --agent opencode --tools "<task>"` (defaults to kimi-k3.1-pro
  at high effort, routed).

Relay the output. `--background` → relay the `job <id>` line and name
`claude-code opencode status|result|cancel`. Replays: `claude-code ask --log` / `ask --retry <id>`
still work.
