# Embedded Templates

> Every file the binary ships via `//go:embed` (`grep -rn "//go:embed" internal cmd`), where it
> renders from, and what runs it. The link column points at the real source file — never a copy.
> `examples/` holds generated outputs for the ones worth seeing filled in, each guarded by a golden
> test in the owning package (`-update` rewrites it; see each test's doc comment for the command).

| Template | Source | Rendered by | Example |
| --- | --- | --- | --- |
| Memory bank seed files (6) | [`internal/memory/templates/*.md`](../../internal/memory/templates/) | `claude-code memory init` (`Init`), `claude-code memory index` (region/catalog) | [memory/sessionHistory.md](examples/memory/sessionHistory.md), [decisionLog.md](examples/memory/decisionLog.md), [activeContext.md](examples/memory/activeContext.md) |
| Settings sections (8) | [`internal/settings/template/*.json`](../../internal/settings/template/) | `claude-code generate` (alias `settings`), baked into `claude-code install` | [settings/settings.json](examples/settings/settings.json) |
| Provider defaults | [`internal/providers/defaults.json`](../../internal/providers/defaults.json) | `claude-code profile` (alias `providers`), `claude-code switch`, `claude-code models` | — |
| Status line default config + palettes | [`internal/statusline/default_config.json`](../../internal/statusline/default_config.json), [`default_palettes.json`](../../internal/statusline/default_palettes.json) | `claude-code statusline` (invoked by `settings.json`'s `statusLine.command`) | — |
| Vendor config defaults | [`internal/vendorconfig/defaults.json`](../../internal/vendorconfig/defaults.json) | `claude-code agents sync`, `claude-code generate --vendors` | — |
| tmux.conf | [`internal/launch/tmux.conf`](../../internal/launch/tmux.conf) | `claude-code install` (`seedTmuxConf`; only when no tmux config exists) | — |
| VS Code machine-settings template | [`internal/vscode/machine-settings.template.json`](../../internal/vscode/machine-settings.template.json) | `claude-code vscode` | — |
| VS Code keybindings template | [`internal/vscode/keybindings.template.json`](../../internal/vscode/keybindings.template.json) | `claude-code vscode` (keybindings leg) | — |
| claude-science config | [`internal/launch/science.toml`](../../internal/launch/science.toml) | `claude-code science` (`--print-env`) | — |
| wl-paste WSL clipboard proxy | [`internal/clipboard/wl-paste-claude.sh`](../../internal/clipboard/wl-paste-claude.sh) | `claude-code install` (`InstallProxy`, WSL only) | — |
| Web dashboard page | [`internal/web/assets/index.html.tmpl`](../../internal/web/assets/index.html.tmpl) | `claude-code web` (`GET /`) | — |

## Settings — the Windows subset

`claude-code install` also renders a Windows-only subset of the same settings template (drops
`hooks`/`statusLine` and any Linux-path string) via `internal/launch/windows_settings.go`'s
`stripWindowsOnly`, written to `%USERPROFILE%\.claude\settings.json` on WSL. Example:
[settings/settings.windows.json](examples/settings/settings.windows.json).

## Memory — generated, not seed templates

Two more memory outputs are generated (not seeded from `templates/*.md`) and are worth an example:

| Output | Built by | Example |
| --- | --- | --- |
| Store catalog (`memory/index.md`) | `BuildCatalog` (`internal/memory/region.go`), run by `claude-code memory index` | [memory/index.md](examples/memory/index.md) |
| A repo's `MEMORY.md` bank region | `buildBankRegion` (`internal/memory/region.go`), run by `claude-code memory index` | [memory/MEMORY.md](examples/memory/MEMORY.md) |
