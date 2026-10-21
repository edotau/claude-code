---
description: "Invoke the in-process Gemini bridge for long-context code exploration, analysis, and documentation generation; `review` mode is a whole-module second opinion"
allowed-tools: Bash, Glob, Read
argument-hint: "[review] [--model name] [--dirs path,...] [--files pattern,...] [--index] [--diff] [--rubric file] [--format text|json] <task>"
---

# /gemini — Gemini Bridge (native Go)

Launcher only. Flags, defaults, the review-mode contract, error handling and prompt scaffolds live
in `agents/gemini.md`; full flag reference: `claude-code gemini bridge --help`.

Parse `$ARGUMENTS`: a leading `review` word selects review mode (whole-module second opinion,
pinned to the opus tier, per `agents/gemini.md`); otherwise run the bridge directly:

```bash
claude-code gemini bridge [flags parsed from $ARGUMENTS] -- "<task>"
```

Relay the bridge's output. For `review` mode, relay findings under a `## Gemini second opinion`
heading plus your own one-paragraph assessment, per `agents/gemini.md`.
