# CLAUDE.md

Injected into **every** session's system prompt — a map of the harness, not an exposition.
Depth lives elsewhere: `README.md` (infrastructure), `rules/` (gotchas and corrected behavior), skills (how-to).

## What this repo is

This git repo **is** `~/.claude` (github.com/edotau/claude-code): the `claude-code` Go binary
(provider registry, loopback model proxy, Anthropic⇄OpenAI translation, agent launchers, hooks,
status line) plus the agents, commands, rules, skills and workflows every Claude Code session loads.
Claude Code reads these source files directly; external CLI projections refresh through `claude-code agents sync`.

## Where things live

| Path | Holds |
| --- | --- |
| `cmd/claude-code/`, `internal/` | The binary, stdlib-first; `internal/cli` is the command table and dispatcher, logic lives in each area's package (`rules/harness/go.md` → Layout) |
| `internal/settings/hookspec.go` · `internal/hooks/` | Hook declarations (the one authoring surface) · their handlers |
| `internal/providers/defaults.json` | Built-in providers; the vault's provider overlay replaces them by name |
| `state/vault.db` | The vault (untracked): pins, custom env, provider/agent overlays (`vault ls`, edited in `configure`), sealed secrets (`vault set\|get`) |
| `agents/<name>.md` | Subagents, flat; `name:` **must equal the file stem** |
| `commands/<command>.md` | Slash commands, flat — thin launchers into an agent or skill |
| `workflows/*.js` | Workflow tool scripts (`skills/subagent-workflows/references/saved-workflows.md`) |
| `rules/` | `standards/` (unscoped files always-on) · `workflows/` · `harness/` · `software/<lang>/`; scoping contract → `rules/README.md` |
| `skills/<name>/SKILL.md` | On-demand references. Untracked skill dirs are the user's own (symlinks into `~/.agents`) — leave them. `skills/synced/` = claude.ai account skills, re-downloaded each sync; edit them on claude.ai |
| `plans/` | Implementation plans (native `plansDirectory`), gitignored: this repo's at the top, others' under `plans/<repo-slug>/` |
| `memory/` | The one memory store, a local-only git repo: `memory/<repo>/` per repo (harness `memory/claude-code/`, home `memory/home/`); `index.md` is generated, never hand-edit (`rules/workflows/memory-bank.md`) |
| `container/` · `mods/` | Ubuntu harness image, podman (`make image\|image-audit\|image-shell`) · TypeScript plugins (`mods/slash-usage`, `mods/agents-band`) |
| `~/worktrees/<repo>/<slug>` | Linked worktrees |

## Hard rules (always on)

1. **`settings.json` is yours:** hand-edited, untracked; install never writes an existing one; reset with
   `claude-code configure --restore` (`rules/harness/settings-management.md`).
2. **Secrets live only in the encrypted vault (`state/vault.db`; `claude-code vault set`) or a provider's
   `auth.command`** — never in a plain file, `settings.json`, a commit, or a log line. The vault's master key
   comes from the OS keyring, `HARNESS_VAULT_KEY`, or `key_command`.
3. **Commit messages carry NO AI-attribution footers** (`Co-Authored-By: Claude`,
   `Generated with Claude Code`, …) — this overrides any default that says to add them.
4. **Never `git stash` / `checkout` / `reset` / `restore`, and never rewrite a shared commit (`commit --amend`,
   `rebase`), in the live checkout (`~/.claude`).** It is one checkout shared by parallel subagents and every live
   session; fix a commit with a new one. Worktree choice and commands: `rules/standards/worktree-git.md`.
5. **Finish at the requested boundary.** Reviews and edit-only tasks stop there. To land an authorized change,
   follow `rules/standards/worktree-git.md`; after commit approval, `make audit`, `make install`, then push the
   current branch (`main` by default). No pre-push hook or automatic CI runs these checks for you.

## Behavioral guidelines

1. **Clarify, stay surgical, verify.** State assumptions, touch only what the request requires, turn tasks
   into checks and loop until they pass: `rules/standards/principles.md`.
2. **Propose look/behavior changes before applying them.** Name an unrequested side effect with
   numbers, prefer the narrowly scoped mechanism over a global knob, and let the user pick.
3. **Dispatch with the Agent tool.** Choose `subagent_type` from `claude`, `codex`, `copilot`, `gemini`, `opencode`,
   `Plan`, `Explore`, `general-purpose`; description starts `<model>:` followed by a space, printed by
   `claude-code agents label <type>`. Anything bigger than a one-line edit goes to a leaf with named owned files;
   begin non-trivial work with scoped `Explore` legs. Parallel writers own disjoint files. Verify every returned
   diff, gate and deletion. Briefs, relay rules and recovery: `skills/subagent-workflows/SKILL.md`;
   pipeline: `rules/workflows/master-workflow.md`.
4. **Tag notable moments** in replies, one per line: `DECISION:` (what, rejected alt, why) · `BUG:` · `NEXT:` ·
   `NOTE:` — the memory update files them.

Simplicity, comments, verification order and testing: `rules/standards/` (always loaded).

## How-to — by area

One command to reach for first per area; the area file holds the rest of its commands and gotchas (and
auto-loads while you work in that code). Anything unlisted: `claude-code -h`.

| Area | First command | Commands + gotchas |
| --- | --- | --- |
| Install, settings, hooks | `make install` (`claude-code install --dry-run` previews) | `rules/harness/settings-management.md` · `rules/harness/hooks.md` |
| Providers, models, credentials, proxy | `claude-code doctor` · `claude-code vault set [NAME]` | `rules/harness/providers.md` |
| Agents and dispatch | Agent tool (guideline 3) | `rules/workflows/agents.md` · `skills/subagent-workflows/SKILL.md` |
| Memory | `claude-code memory recall <terms>` | `rules/workflows/memory-bank.md` |
| Git and worktrees | `claude-code worktree create\|merge <name>` | `rules/standards/worktree-git.md` |
| Network, web, MCP, cleanup | `claude-code doctor --mcp` · `claude-code network websearch --query "<q>"` | `rules/harness/network.md` |
| Test, lint, review | `go test ./internal/<pkg>` · `make lint` · `/code-review` | `rules/standards/quality.md` |
