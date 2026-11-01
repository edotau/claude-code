---
description: "Gemini via `claude-code gemini <verb>`: a bare task runs the 1M-context bridge, `review` is the whole-module second opinion (opus model), `ask|resume|explore|transfer|status|tail|result|cancel|setup|catalog|profile|on|off` pass straight through"
allowed-tools: Bash, Glob, Read
argument-hint: "[profile=<p>] [model=<m>] [review|<verb>] <task>  (bare = bridge)"
---

# /gemini — Gemini Agent (native Go)

Launcher only. Backends, verbs and the report contract live in `agents/gemini.md`; `claude-code
gemini -h` lists the surface.

`profile=<p>` / `model=<m>` anywhere in `$ARGUMENTS` (strip them first) become `--profile <p>` /
`--model <m>` on the `claude-code gemini <verb>` call or the routed `ask` (not on `profile`/`on`/`off`); a verb that
lacks the flag errors, so relay that. `<p>` is a vault profile (`dev`, `qa`, `prod`, …), `<m>` an alias or wire id.

Parse the first token of `$ARGUMENTS`:

- `review` — whole-module second opinion pinned to the opus model (unchanged): run the bridge
  directly per `agents/gemini.md` → Review mode. Diff review on the gemini model family instead is
  `claude-code gemini review` run directly — say so in one line if that's what's meant.
- `ask|resume|explore|transfer|status|tail|result|cancel|setup|catalog|profile|on|off` — run
  `claude-code gemini <verb> <rest>` straight through.
- anything else — run the bridge as today: `claude-code gemini bridge [flags parsed from
  $ARGUMENTS] -- "<task>"`.

Relay the output. For `review` mode, relay findings under a `## Gemini second opinion` heading plus
your own one-paragraph assessment, per `agents/gemini.md`.
