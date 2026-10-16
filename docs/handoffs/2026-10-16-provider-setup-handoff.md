# Handoff 2026-10-16 — provider setup, Gemini, opencode, codex render

## Done (uncommitted unless noted)
- Stale harness skill copies moved out of `~/.agents/skills` → `~/.agents/stale-harness-skills-20261016/`
  (4 nested under `using-git-worktrees/`); Gemini skill-conflict warnings gone.
- Gemini defaults → opus `gemini-4-argon`, sonnet `gemini-3.8-flash` (committed by user, 26827d9).
  Free-tier key: every Pro model 429s; Flash ≤3.8 works.
- opencode: `tui.json` theme `system` seeded by `generate` (committed); `~/.config/opencode/opencode.jsonc`
  = `google/gemini-3.8-flash`, apiKey `{env:GEMINI_API_KEY}` (live `opencode run` ok).
- Uncommitted: `generate` skips codex on its own (passthrough) login; `vendors.fallbackTarget` treats a
  `model` in opencode.jsonc as opencode's own setup (no opencode.json render); mcpx TestCollectDefs
  resolves macOS /private/var. `make lint` + full `make test` green; mutation-checked.
- Session provider pinned to `subscription` (user's choice).

## Open
- Commit: `fix(generate): skip codex on its own login; opencode honors its own config model` +
  `test(mcpx): resolve the macOS /private/var tempdir` — awaiting user OK.
- User: set OPENAI_API_KEY (`read -rsp … | claude-code secrets set OPENAI_API_KEY`); rotate exposed keys.
- VS Code TUI selection: fullscreen already supports drag-select + Option-drag native; nothing disables
  mouse. Waiting on user to say what fails; options `/tui default` or `CLAUDE_CODE_DISABLE_MOUSE_CLICKS=1`.
- `CLAUDE.md` how-to lists `claude-code profile`; verify the verb exists in the current binary.
