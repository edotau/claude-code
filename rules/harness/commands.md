---
paths:
  - "internal/cli/**"
  - "internal/cli/ui/**"
  - "internal/dev/setup/**"
  - "mods/**"
---
# Command Reference

Use `claude-code <verb> -h` for flags; this file maps tasks to their owning commands and rules.

## Install options

- `rtk make bootstrap`: install Go without sudo, harness, completion, then configure; `CHECK=1` checks instead.
- `claude-code install --dry-run`: preview tools. Existing settings remain owner-managed and are only validated.
- `rtk make install`: install missing agent CLIs/Tailscale; `TOOLS=1` updates all, `TOOLS=copilot,codex` selects,
  then doctor runs. `claude-code install --tools=<name>` does the tool work without doctor.
- Vendor installers scrub credentials, install under `~/.local/bin` and link `bin/<agent>`; verify version or route.
  Host-tool policy: `settings-management.md`; git/go/make/curl are report-only.
  `DEV_TOOLS=1` installs pinned gate tools by default; zero skips them.
- `bin/<agent>` is the unrouted CLI except `bin/claude`, the `claude-code claude --launcher` wrapper;
  native Claude is `~/.local/bin/claude`. Routed launches use `claude-code <agent>`.
  The wrapper applies the session profile overlay and embedded autoMode rules. A failed pinned route fails closed;
  only plain Anthropic passthrough may fall back to native Claude without harness settings.

## Setup

- `claude-code configure` reuses env, vault and CLI logins and asks only for missing values.
  Pages: provider, models, agents, github, remote, codex, routing, databricks; `--check` reports missing values.
- `configure vscode [--check|--dry-run]`: merge the settings and keybindings templates into VS Code, with backups;
  flags only. Templates: `internal/assets/ui/vscode/`.
- `configure codex`: effort, verbosity and config_route in the vault.
- `configure provider <p> options.warehouse_id=<id> options.prices='<json>'`: Databricks costs in USD/million tokens.
- `configure routing weights.price=0.4 weights.latency=0.2`: headroom fills to 1 (defaults 0.4/0.4/0.2).
- `configure routing spendCap.sessionUSD=<usd> spendCap.dayUSD=<usd>`: off until set; empty clears.
  At cap, unmetered subscription/CLI rows remain; no remaining route fails. See `providers-reference.md` → Spend cap.
- Claude subscription keeps Remote Control; API keys pay per token. For headless Max, configure mints
  `claude setup-token` into vault `CLAUDE_CODE_OAUTH_TOKEN`.
- `configure restore [--agent a|all]` resets from templates with backups; `configure revert [--agent a,b]`
  restores CLI config backups. Configs merge, never replace, with five backups each (`agent.md`).
  Worktree-only resets: `settings-management.md` → Worktree render.
- Existing `~/.claude`: back up user data and resolve collisions manually before installing a checkout;
  never overwrite credentials or user files. Use `make bootstrap` for a fresh checkout.

## Agents

Dispatch syntax and lifecycle: `skills/subagent-workflows/references/agent-verbs.md` and `cost-routing.md`.
Authoring and cloud opt-in: `rules/workflows/agents.md`.

- `claude-code agents list`: CLI availability; `agents ask --overlay cfg.json --brief task.json --dry-run`:
  resolved route/argv without a model call. Tools default on unless overlay `tools:false` or `noTools:true`.
- `agents ask --batch tasks.jsonl|- [--jobs N] [--fail-fast]`: pool dispatch.
- `claude-code opencode catalog list|add|remove|import|reset|check|sync`: shared Databricks model menu.
- `claude-code usage --days 7`: tokens; `--cost`: estimated profile/model cost; `--context`: session sources.
- `agents sync` renders under `state/projections/<agent>/`, installs into agent homes and links skills into
  `~/.agents/skills` plus Gemini/Copilot skill dirs. Never put AGENTS.md at this root.
  No agent gets links to `skills/synced/**`; OpenCode reads it natively.
  Envelope/instruction behavior: `agent.md`; fan-out: `rules/workflows/master-workflow.md`.

## Status line

`claude-code configure theme statusline [--style <name>]`: gradient, powerline, capsule, minimal, dashboard, twoline.
It shows provider/profile, model, context, git and weekly/token limits.
`~/.claude/state/statusline/config.json` controls `segments`: false disables a key.
Default off: proxy, duration, session. Other keys: path, model, provider, context, limits, total, git, velocity.

## Layout

- `bin/.hookd` and `bin/claude-code` share `bin/.build-stamp`; see `hooks.md`.
- `state/vault.db`: untracked secrets; `state/certs/`: CA bundles; `state/harness/<owner>/`: schema-versioned state
  migrated on read; `state/harness/backups/`: plaintext backups via `paths.Backup`. Doctor reports formats.
- Outside state: machine-wide `/tmp/claude-code-bench-<uid>` lock, `$TMPDIR/claude-code-verify` trees,
  `~/.claude/run/clip.sock`, bin, plans, memory, settings and vendor homes.
- `docs/templates/` indexes embedded templates. `container/` holds the Ubuntu podman image
  (`make image|image-audit|image-shell`); rootless Linux installs Tailscale static binaries without starting its daemon.
- TypeScript mods: slash-usage, dispatch-spawn, codemode (`/codemode on|off|status`); build with `tsc -p mods/<name>`.
  agents-band is unloaded by default; add its path to `CLAUDE_CODE_PLUGIN_DIRS`, then `/reload-plugins`.
  It shows leaf claims/notes and free dirty files (`agents tree --files`).

## Other commands

- Completion: `claude-code completion --install`.
- OpenCode effort: TUI `ctrl+t`, `opencode run --variant high`, or ask overlay `effort`.
- Worktrees: `skills/using-git-worktrees/SKILL.md`; diff/find/status: the agent-verbs reference above.
- Memory sandbox: `rules/workflows/memory-bank.md` → Verbs.
- Remote execution, browser, web, MCP and cleanup: `network.md`.
- Databricks metadata: `claude-code databricks api /api/2.0/serving-endpoints --profile dev [--as user|sp]`.
- Open-Jev classifier: `claude-code gateway jev` (flags only; bare = `--status`: server, served model, job, passes).
  `--train [--bg|--fg|--fresh|--labels DIR|--tasks LIST] [-- trainer options]` starts detached and watches
  (Ctrl-C only detaches; `--bg` skips the watch, `--fg` runs in the terminal), `--attach`, `--stop`, `--options`;
  `--trainer DIR --out DIR` are remembered after first use. `train-jev.sh` is a thin front for it; `doctor` has a
  `jev` row.
- Housekeeping: `rtk make sweep [DRY_RUN=1] [AGE=7d] [ONLY=<targets>]` removes; preview first as needed.
  `doctor --audit [--days 7]`, `configure history snapshot|log|diff`.
- Theme: `configure theme [--palettes|--reset] [<palette>]`, `configure theme apply`.
- `claude-code tailscale`: one of `--machines`, `--connect`, `--token [--secret|--save]`,
  `--exit-node on|off|approve`, `--subnet <cidr>[,…]|approve|off`, `--route <peer>|off`.
  Token retrieval needs the owner's terminal, browser re-auth and vault OAuth client credentials or API key.
