# theme/ — one look, every tool

One JSON file per tool, holding only the keys that set its appearance. `claude-code theme apply` merges
each file over that tool's own config (foreign keys kept, a `.bak-<stamp>` under `~/.claude/state/harness/backups/`
for any file it rewrites); `--dry-run` lists what would change. Edit a file here, apply, and the box matches again.

| File | Merged into | Keys |
| --- | --- | --- |
| `vscode.json` | VS Code user `settings.json` (`~/Library/Application Support/Code/User` on macOS) | `workbench.colorTheme`, `workbench.colorCustomizations` (the #300a24 panels + terminal ANSI ramp), font sizes, cursor |
| `statusline.json` | `~/.claude/state/statusline/config.json` | gradient palette, color mode, powerline theme (`claude-code theme <name>` changes just the gradient) |
| `opencode.json` | `state/harness/opencode/tui.json` (the tracked overlay opencode launches are pointed at) | `theme` (`gemini-plum`, the owner's custom theme) |
| `opencode-themes/gemini-plum.json` | `~/.config/opencode/themes/gemini-plum.json` | the theme file itself; opencode only reads custom themes from its own config dir, so this is the one opencode file the harness still writes |
| `copilot.json` | `~/.copilot/settings.json` | `theme` (`default` keeps the terminal background; `github` forces #0d1117) |
| `gemini.json` | `~/.gemini/settings.json` | `theme` |
| `ghostty.conf` | `~/.config/ghostty/config` | `theme` (Ubuntu), background/foreground colors, cursor style/color, window padding and behavior, font settings, clipboard permissions |

Claude Code's own `"theme": "auto"` lives in `internal/settings/template/` (rendered by `make install`),
not here. Codex's `appearanceTheme` is set from the ChatGPT desktop app and is left alone.
