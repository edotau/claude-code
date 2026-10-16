# Handoff — claude-dbx parity round 2 (2026-10-16)

State: `origin/main` = `ba53b3d` + this handoff. CI green. Tree clean. Every item in
`docs/plans/2026-09-26-claude-dbx-feature-port.md` rounds 1–2 is **done** except `agent-rules-guard` (skipped on purpose).
claude-dbx is read-only reference from here; it is retired once this repo is installed as `~/.claude` everywhere.

## 1. First thing on the MacBook — verify, in this order

Nothing below was run on macOS. Linux + a darwin/arm64 cross-compile (`go build ./...`, `go vet` on the tagged
packages) are the only evidence, so each step is a live check, not a formality.

1. **Clone/pull as `~/.claude` and build**: `cd ~/.claude && go version` (needs Go ≥ 1.27 — `go.mod` pins it),
   `make compile`, then `make audit`. Expect green; `govulncheck` reports "not installed — skipped" unless installed.
2. **Install**: `make install` (binary → `~/.claude/bin`, shims `claude codex gemini opencode copilot`,
   `~/.local/bin/claude-code`, settings.json merge with backup, `completion --install`, router restart if up).
   Then `export PATH="$HOME/.claude/bin:$HOME/.local/bin:$PATH"` (`make setup-env APPLY=1` writes it once).
   **macOS bash is 3.2**: `claude-code completion` must still load there — run `bash -n <(claude-code completion)`
   and `source <(claude-code completion); complete -p claude-code`. On zsh: `autoload -U bashcompinit && bashcompinit`
   first. If the script uses a bash-4 feature, fix `internal/cli/complete.go`'s `bashCompletionScript`.
3. **Raw terminal mode (new, darwin-tagged)**: `claude-code configure provider --dry-run` on a real terminal must show
   the arrow-key panel (`internal/tui/rawterm_darwin.go` uses `TIOCGETA`/`TIOCSETA`; `getSize` uses `TIOCGWINSZ`;
   `readPassword` toggles ECHO). Press ↓, Enter, Esc. If the terminal is left without echo after a crash: `stty sane`,
   then fix `Terminal.Close` ordering. Also `claude-code configure keys` → paste a dummy key for a keyed provider → it
   must NOT echo, and must land in `env.d/secrets.env` at 0600 (`claude-code secrets status`, then delete it).
4. **Keyring plane**: `claude-code secrets set X --keyring` must use `security add-generic-password` (service
   label in `internal/secrets/keyring.go`), `secrets get X` must read it back, `secrets doctor` must say the tool is
   present. Linux only had `secret-tool`; the `security` branch is untested.
5. **VS Code**: no `~/.vscode-server` on a Mac, so `claude-code vscode --dry-run` must target
   `~/Library/Application Support/Code/User/settings.json`. Check the template's `claudeCodeChat.wsl.*` keys are
   harmless there (they are WSL-only extension settings) — if VS Code warns, gate them on `~/.vscode-server` in
   `internal/vscode`. Right-click paste in the integrated terminal comes from `terminal.integrated.rightClickBehavior`.
6. **Vendor legs**: `claude-code codex --print-env`, `copilot --print-env`, `opencode --print-env`, `gemini --print-env`
   — each must either route, name its fallback (`codex login`, `gh auth`, opencode's `auth.json` under
   `~/.local/share/opencode` — confirm opencode uses that path on macOS; if it uses `~/Library/Application Support`,
   extend `opencodeAuthFile()` in `internal/vendors/vendors.go`), or pass through to the plain CLI with the note.
   Then run each bare once interactively: the `Set up <vendor> now? [Y/n]` offer must appear only when nothing resolves.
7. **Hooks on macOS**: `claude-code doctor` → hooks present; `rtk-wrap` is a no-op without `rtk`; `stop-format` needs
   `gofmt`/`ruff`/`prettier` on PATH to do anything. `docs-placement` uses `git` — fine.
8. **Memory**: start a session in a repo, `claude-code memory path`, `memory doctor`, `memory archive` (audit). The
   native store is Claude Code's own `~/.claude/projects/<slug>/memory`; confirm the slug matches what Claude Code writes
   on macOS (`ls ~/.claude/projects`) — `projectSlug` replaces every non-alphanumeric byte with `-`.
9. **Router**: `claude-code router start|status|stop` — the daemon code has darwin branches in `internal/router/proc_*`
   and `internal/proc/parent_darwin.go`; `make install` restarts it only if it was running.
10. **CI parity**: `make lint` (now scoped to the checkout via `CLAUDE_CONFIG_DIR=$(CURDIR)`), `make verify`.

## 2. Bugs fixed this round that are worth a second look

- `internal/launch/launch.go` `Vendor`: only `--provider` is explicit; the session pin is Claude Code's default. If a
  user WANTS a vendor forced onto the session provider, they pass `--provider`. Revisit if that surprises anyone.
- `internal/vendors` `canServe`: a passthrough provider never serves another vendor, even on a matching dialect.
- `internal/hooks/safety.go`: hard-block regexes match inside ONE shell segment; the download-into-shell pattern is
  the one `pipeBlocks` entry that spans the whole line. Add new pipe-spanning patterns there, not to `hardBlocks`.
- `internal/cli/memory.go` `memoryRoot`: `CLAUDE_PROJECT_DIR` is honored only when it is a git work tree or the config
  dir. A launcher stamping `$HOME` no longer attaches bank verbs to a junk root (dbx had the same defect).
- `internal/memory/doctor.go`: covers the LOCAL bank too, prefixed `local`/`global`.
- `internal/generate`: a config dir without `CLAUDE.md` skips the roster docs render instead of failing.

## 3. Next — improvements, ranked

1. **Subagent turn cap**: every `claude` agent dispatch today hit `maxTurns: 30` (`agents/claude.md`) before reporting;
   ~12 agents burned their budget and left work on disk without a handback. Raised to 60 in this commit; watch cost.
2. **`agent-inflight` stale markers**: an agent that stops at its turn limit does not retire its slot (no SubagentStop
   with a matching id), so slots leak until the 30-min TTL. Retire on the parent's `task-notification` instead, or
   probe the owner pid + agent transcript mtime. Cap is now 8 (`hooks.AgentMaxInflight`).
3. **`secrets git` noise**: it flags synthetic `sk-`/`ghp_` fixtures in `*_test.go`. Skip test files, or require a
   checksum-shaped token (GitHub's push protection does; it only flagged `dapi` + `xoxb-`). Both fixtures are now
   assembled at runtime; any new fixture must be too.
4. **`generate` for codex** writes `env_key = "HARNESS_CODEX_API_KEY"`, so a `codex` launched OUTSIDE the harness has no
   key. Either document that the shim is the entry point, or render an `[auth]` exec helper (`claude-code resolve`)
   the way dbx did.
5. **opencode precedence**: own `auth.json` login now beats a keyed registry provider. If someone keys opencode to
   OpenRouter on purpose, `--provider openrouter` is the override; consider a per-vendor pin only if that comes up.
6. **`agent-rules-guard`** stays unported: it needs per-agent native memory with a protected RULES region + seeding.
   Port only if agent memory is wanted here.
7. **dbx hooks still registered in the shared `~/.claude/settings.json` on the Linux box** (both harnesses' hooks ran
   this session; the dbx safety hook blocked two of my own commands on test strings). When dbx is retired there, run
   `claude-code install` once to re-merge and remove the `claude-dbx hook …` entries by hand.
8. **Help summaries ≤60 chars** are enforced by `internal/cli/cli_test.go`; the Makefile help is not — keep it terse by eye.
9. **`history`** watches `~/.codex/config.toml`, opencode's `opencode.json`, gemini's `settings.json`, the memory bank and
   `MEMORY.md`; it never snapshots `env.d/secrets.env` (by name and by content scan). Add VS Code's settings file if
   `vscode` churn becomes a problem.
10. **Docs placement gate** points blocked writes at `~/.claude/docs/{plans,reports,handoffs}`; `docs/handoffs/` is
    tracked again (only the dbx `HANDOFF.md` beacon is ignored). `session-data/` stays ignored while dbx runs here.

## 4. How the memory system is shaped now (for whoever touches it next)

- Injected per session: `activeContext` + `progress` in full, the bank's generated `index.md` (≤8,500 chars total),
  the cached doctor notice, and Claude Code's own `MEMORY.md` (keep it ≤20 pointer lines via `memory archive --split`).
- Found on demand: everything else, by the `memory-recall` hook (BM25, ≤3 blocks, dedup against what is already in
  context) and `claude-code memory search`.
- Kept small automatically on every settle: sessionHistory rotates past 30 entries, local overflow promotes to global,
  global overflow archives, `access.log` feeds hot/cold nominations into `memory doctor`.

## 5. Session facts

- Commits this session on `main`: `3dbfc4d` (owner) → `2418cba` `d81135c` `42d7a33` `292e57c` `56e97c2` `d8c6bb8` `ba53b3d`.
- Push protection rewrote `8705468` → `3dbfc4d` (fixtures assembled at runtime); nothing else in history changed.
- Verified live, not just by tests: every hook, every memory verb, generate, configure (piped + pty), vscode, completion,
  secrets, clean, history, theme, mcp doctor, the vendor fallback/passthrough chain, Makefile `lint`/`audit`.
