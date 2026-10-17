# theme/ — one look, every tool

One JSON file per tool, holding only the keys that set its appearance. `claude-code theme apply` merges
each file over that tool's own config (foreign keys kept, a `.bak-<stamp>` beside any file it rewrites);
`--dry-run` lists what would change. Edit a file here, apply, and the box matches again.

| File | Merged into | Keys |
| --- | --- | --- |
| `vscode.json` | VS Code user `settings.json` (`~/Library/Application Support/Code/User` on macOS) | `workbench.colorTheme`, `workbench.colorCustomizations` (the #300a24 panels + terminal ANSI ramp), font sizes, cursor |
| `statusline.json` | `~/.claude/statusline/config.json` | gradient palette, color mode, powerline theme (`claude-code theme <name>` changes just the gradient) |
| `opencode.json` | `~/.config/opencode/tui.json` | `theme` (`system` follows the terminal background) |
| `copilot.json` | `~/.copilot/settings.json` | `theme` (`default` keeps the terminal background; `github` forces #0d1117) |
| `gemini.json` | `~/.gemini/settings.json` | `theme` |

Claude Code's own `"theme": "auto"` lives in `internal/settings/settings.tmpl.json` (rendered by `make install`),
not here. Codex's `appearanceTheme` is set from the ChatGPT desktop app and is left alone.
