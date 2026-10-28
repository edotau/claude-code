---
description: "Gemini via `claude-code gemini <verb>`: a bare task runs the 1M-context bridge, `review` is the whole-module second opinion (opus model), `ask|resume|explore|transfer|status|tail|result|cancel|setup|catalog|env` pass straight through"
allowed-tools: Bash, Glob, Read
argument-hint: "review|ask|resume|explore|transfer|status|tail|result|cancel|setup|catalog|env <args> | [--dirs d] [--files f] <task>"
---

# /gemini — Gemini Agent (native Go)

Launcher only. Backends, verbs and the report contract live in `agents/gemini.md`; `claude-code
gemini -h` lists the surface.

Parse the first token of `$ARGUMENTS`:

- `review` — whole-module second opinion pinned to the opus model (unchanged): run the bridge
  directly per `agents/gemini.md` → Review mode. Diff review on the gemini model family instead is
  `claude-code gemini review` run directly — say so in one line if that's what's meant.
- `ask|resume|explore|transfer|status|tail|result|cancel|setup|catalog|env` — run `claude-code gemini <verb> <rest>`
  straight through.
- anything else — run the bridge as today: `claude-code gemini bridge [flags parsed from
  $ARGUMENTS] -- "<task>"`.

Relay the output. For `review` mode, relay findings under a `## Gemini second opinion` heading plus
your own one-paragraph assessment, per `agents/gemini.md`.
