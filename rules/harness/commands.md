---
paths:
  - "internal/cli/**"
  - "internal/ui/**"
  - "internal/setup/**"
  - "mods/**"
---
# Command Reference

Moved from the README: install options, setup, agent and utility commands, the status line and the tree layout.

## Install options

- `make bootstrap` — fresh box: Go toolchain (no sudo), install, bash completion, then `claude-code configure`
  (`CHECK=1` runs `configure --check` instead).
- `claude-code install --dry-run` previews, listing the host tools it would install; an existing `settings.json` is
  kept as yours and only checked for validity.
- `make install` installs any missing agent CLI and Tailscale, never updating one already present
  (`claude-code install --tools --missing`). `TOOLS=1` installs or updates them all and runs `claude-code doctor`
  (`TOOLS=copilot,codex` picks); `claude-code install --tools=<name>` is the same without the check. Each vendor
  installer runs with credentials scrubbed and puts its binary in `~/.local/bin` (on `PATH`); install links it as
  `bin/<agent>`. Confirm with `<agent> --version`, the doctor
  `<agent> route:` line, or `claude-code agents ask --agent <agent> "Reply with exactly: ok"`.
- The same step installs missing host tools (shfmt, ruff, node, prettier, gh, tmux, shellcheck) through `go install`,
  `npm --prefix ~/.local`, uv/pipx or, as the fallback, brew, and runs no root command; git, go, make, curl are
  report-only, and `DEV_TOOLS=1` (default) also `go install`s three pinned linters (`DEV_TOOLS=0` skips). `claude-code
  doctor` prints a `dep <name>` row per tool with the command that installs it, and a failed install never fails `make
  install`.
- `bin/<agent>` is the real, unrouted CLI. Routed launches go through `claude-code <agent>`.

## Setup

```bash
claude-code configure                 # interactive menu: agents, tools (databricks, tailscale, web search), settings
                                      # (secrets, routing, environment, seed agents); Submit seals keys
claude-code configure models          # one page (provider|models|agents|github|remote|codex|routing|databricks)
claude-code configure codex           # codex effort and verbosity (vault)
claude-code configure --check         # report only; exit 1 if anything is unset
claude-code configure --restore [--agents|--agent a]   # reset settings.json to the template (backup first) + agent configs
claude-code configure --revert [--agent a,b]        # restore agent CLI configs from backup
```

- `configure` reuses what already exists (env, vault, agent CLI logins) and prompts only for what is missing.
- **Claude subscription** keeps Remote Control; **API key** pays per token. For headless runs on a Max plan,
  `configure` mints `claude setup-token` into the vault as `CLAUDE_CODE_OAUTH_TOKEN`.
- Agent CLI configs are merged, never replaced, with 5 backups per file; details: `rules/harness/agent.md`.
- From a worktree, reset only that checkout:
  `CLAUDE_CONFIG_DIR="$PWD" claude-code configure --restore --out "$PWD/settings.json"`
  (`--out` never renders agent CLI configs, and `--out` with `--agents` is refused).

- `~/.claude` already exists: clone into `~/.claude.stage`, then `make -C ~/.claude.stage adopt` (backs up
  collisions, then bootstraps). `install --setup nuke [--apply] [--keep-memory]` archives untracked content and
  removes it, leaving credential-shaped untracked or ignored files (`.env`, `*.pem`, `*.key`) in place;
  `install --setup workspace init|link|status` scopes a project's own `.claude/`.

## Agents

```bash
claude-code agents list                                # agents and availability
claude-code agents ask --agent codex "Review this branch."    # one-shot; --format json, --tools, --effort
claude-code agents ask --agents <json|file> --agent <name> "<task>"   # inline agent definitions
claude-code agents status | ask --retry <id>                   # recent dispatches; replay one
claude-code agents ask --batch tasks.jsonl|- [--jobs N] [--fail-fast]   # many tasks on a pool; one envelope per line
claude-code opencode --profile ollama --model qwen3-coder   # interactive; -- forces passthrough
claude-code opencode catalog [list|add|remove|import|reset|check|sync]   # shared Databricks model menu
claude-code codex review --adversarial --base main     # per-agent verbs; claude-code codex -h lists them
claude-code claude review --base main --overlay '{"cloud":true}'   # cloud ultrareview; sends the diff to Anthropic
claude-code codex cloud exec "<task>" --env <id> --overlay '{"cloud":true}'   # Codex Cloud; status|list|diff|apply
claude-code agents ask --agent gemini --tools --dirs internal "<task>"  # one 1M-context sweep via agy
claude-code agents ask --agent codex --overlay '{"provider":"databricks","profile":"auto","remote":"auto"}' --tools "<task>"
                                                       # per-call config
claude-code usage --days 7                             # subagent token usage
claude-code usage --context                          # where one session's context went, by source
```

Without `--tools` the agent's tools are off (per agent: `rules/harness/agent.md`); `--read-only` adds a read-command
allow list. `agents sync` renders under `state/projections/<agent>/`, installs into
`~/.codex`, `~/.copilot`, `~/.gemini`, `~/.config/opencode`, and links `skills/` into `~/.agents/skills` (plus gemini's
and copilot's own skills dirs); no AGENTS.md lives at this root (Claude Code would load it as a second CLAUDE.md).
claude.ai `skills/synced/**` is linked for no agent; opencode reads it from `~/.claude/skills` natively. Envelope
fields, instruction sources and per-agent behavior: `rules/harness/agent.md`; in-session fan-out:
`rules/workflows/master-workflow.md`.

## Status line

`claude-code theme statusline [--style <name>]` (`gradient`, `powerline`, `capsule`, `minimal`, `dashboard`,
`twoline`) shows `<provider>·<profile> · <model>`, context % and git state, plus the weekly limit (`7d:N%`) or a
minted token's time left (`m2m:42m`; labels `u2m`, `m2m`, `pat`). Segments are on unless `config.json` sets
`"segments": {"<key>": false}`; off by default: `proxy`, `duration`, `session`. Others: `path`, `model`, `provider`,
`context`, `limits`, `total` (`1.23M`, `+` while dispatches run), `git`, `velocity`. Config:
`~/.claude/state/statusline/config.json`.

## Layout

`CLAUDE.md` → Where things live. State: `state/vault.db` (untracked), `state/harness/<owner>/` (versioned per the
`internal/schemas` registry; formats migrate on read, `doctor` reports `formats`),
`state/harness/backups/` (every plaintext backup, via `paths.Backup`);
`docs/templates/` indexes every embedded (`//go:embed`) template.
`container/` holds the Ubuntu harness image, built and run with podman (`make image|image-audit|image-shell`); with no
root or `sudo`, Linux installs Tailscale's static binaries into `~/.local/bin` and does not start the daemon.
`mods/slash-usage/` is a TypeScript plugin, built with `tsc -p mods/<name>`. `mods/agents-band/` is kept in the
tree but not loaded; add its path to `CLAUDE_CODE_PLUGIN_DIRS` (then `/reload-plugins`) to try it: a band of live
leaves, each expandable to the files it holds and its latest note, plus the free dirty files (`agents tree --files`;
plain rows on an older binary).

## Other commands

| To… | Run |
| --- | --- |
| Adopt, wipe or scope a `.claude/` | `claude-code install --setup adopt\|nuke\|workspace` |
| Bash tab completion | `claude-code completion --install` |
| OpenCode reasoning effort | TUI `ctrl+t` · `opencode run --variant high` · `claude-code agents ask --effort high` |
| Worktrees under `~/worktrees/<repo>/` | `claude-code worktree list\|create <n>\|merge <n>\|clean <n>\|clean-all` |
| Subagent status and dispatches | `claude-code agents tree [--watch\|--detail]` · `agents cancel --lost [--dry-run]` |
| What an agent changed, in its files, passing the gates, routed right | `claude-code agents diff [--path <p>]... [--check] [--verify <id>]` |
| Search repo code deterministically (tracked and new files, never ignored ones) | `claude-code agents find <pattern> [--name\|--func\|--def\|--callers] [-i] [-w] [-l] [--path <p>]... [--max 200] [--format text\|json]` |
| Merge another machine's memory, try on a copy | `claude-code memory import --host <h>` · `memory sandbox` |
| Run an agent on another tailnet box | `claude-code agents ask --host <h> --agent <a> "<task>"` · `network remote --host <h> --list --status` |
| Browsers, web search | `claude-code network browser [--setup --browser <b>]` · `network websearch --query q` |
| Tailnet | `claude-code network tailscale [--machines\|--connect\|--key --secret\|--save\|--exit-node …\|--subnet <cidr> …]` (`TAILSCALE_OAUTH_CLIENT_ID`/`_SECRET` or `TAILSCALE_API_KEY` in the vault; `make install` adds it when missing) |
| Lint a workflow script | `claude-code workflow lint` · `workflow skeleton <pattern>` |
| Housekeeping | `claude-code clean [--list\|--dry-run]` · `doctor --audit [--days 7]` · `history snapshot\|log\|diff` · `doctor --mcp` |
| Theme, VS Code | `claude-code theme [--palettes\|--reset] [<palette>]` · `theme apply` · `theme vscode [--check\|--dry-run]` |
