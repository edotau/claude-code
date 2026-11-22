# CLAUDE.md

Injected into **every** session's system prompt — a map of the harness, not an exposition.
Depth lives elsewhere: `README.md` (overview, install, safety, design decisions), `rules/` (reference and gotchas),
skills (how-to).

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
| `commands/<command>.md` | Slash commands, flat — thin launchers into an agent (`discover-models` into `claude-code models discover`) |
| `workflows/*.js` | Workflow tool scripts (`skills/subagent-workflows/references/saved-workflows.md`) |
| `rules/` | `standards/` (unscoped files always-on) · `workflows/` · `harness/` · `software/<lang>/`; non-standards rules need `paths:` (`rules/harness/change-protocol.md`) |
| `skills/<name>/SKILL.md` | On-demand references. Untracked skill dirs are the user's own (symlinks into `~/.agents`) — leave them. `skills/synced/` = claude.ai account skills, re-downloaded each sync; edit them on claude.ai |
| `plans/` | Implementation plans (native `plansDirectory`), gitignored: this repo's at the top, others' under `plans/<repo-slug>/` |
| `memory/` | The one memory store, a local-only git repo, one bank for every workspace, no repo keys; `MEMORY.md` and `index.md` are generated, never hand-edit (`rules/workflows/memory-bank.md`) |
| `container/` · `mods/` | Ubuntu harness image, podman (`make image\|image-audit\|image-shell`) · TypeScript plugins (`mods/slash-usage`, `mods/dispatch-spawn`; `mods/agents-band` is kept but not loaded) |
| `~/worktrees/<repo>/<slug>` | Linked worktrees (`<repo>` = checkout dir without a leading dot: `~/worktrees/claude/`) |

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
   follow `rules/standards/worktree-git.md`. Stable code ships continuously: when the committed HEAD builds (macOS,
   `GOOS=linux` build, `GOOS=windows` vet) and its touched packages' tests pass, push to origin main, then install and
   live-check on the Mac (the required check; WSL is a periodic sync, R19). Full `make audit` runs on main after each
   worktree merge (automatic) and at session end; `make install` after pulls.

## Behavioral guidelines

1. **Clarify, stay surgical, verify.** State assumptions, touch only what the request requires, turn tasks
   into checks and loop until they pass: `rules/standards/principles.md`.
2. **Propose look/behavior changes before applying them.** Name an unrequested side effect with
   numbers, prefer the narrowly scoped mechanism over a global knob, and let the user pick.
3. **Dispatch with the Agent tool.** Choose `subagent_type` from `claude`,
   `Plan`, `Explore`, `general-purpose`, `claude-code-guide`, `statusline-setup` (and `web-fetch` where the host
   offers it; else `general-purpose`); external work agents run carried, Pi as a plain ask (see
   `skills/subagent-workflows/SKILL.md` →
   Carried dispatch); description starts `<model>:` followed by a space (`web-fetch` takes none),
   printed by `claude-code agents label <type>`. Anything bigger than a one-line edit goes to a leaf with named owned
   files;
   begin non-trivial work with scoped `Explore` legs. **Fan out as many parallel leaves as the task allows**, in one
   message; the only limits are overlapping files and dependency order. **Keep fanning out:** each time a leaf
   finishes or a file frees up, dispatch every task that is now unblocked, and repeat until none remain. Leaves
   also keep this session's context small: send file reads, diff review and gate runs to a leaf and take back a
   short verdict, with the long report saved at `$(claude-code memory path reports <name>)`. Parallel writers own
   disjoint files. Wave loop (blocked queue, review → fix → commit): `skills/subagent-workflows/SKILL.md`.
   **Before the first dispatch of a multi-part request, write the DISPATCH TABLE** (task → owned files → blocked-by →
   agent) and dispatch every unblocked row in one message; redo it on every hand-back and new user message, merging
   new asks into it, not handling them in arrival order (`skills/subagent-workflows/SKILL.md` → Dispatch table).
   **A bug or issue you notice is fixed right away, without asking:** dispatch its fix leaf at once instead of
   listing it. Verify every returned diff, gate and deletion. Briefs, relay rules and recovery:
   `skills/subagent-workflows/SKILL.md`;
   pipeline: `rules/workflows/master-workflow.md`.
4. **Tag notable moments** in replies, one per line: `DECISION:` (what, rejected alt, why) · `BUG:` · `NEXT:` ·
   `NOTE:` — the memory update files them.

5. **Reply concisely.** Lead with the result; no preamble, narration or closing recap; short by default, full
   detail when asked; never trade correctness for brevity.

6. **Write each call in its final form** so hooks rewrite nothing (a rewrite stalls auto mode): Bash starts `rtk` for
   git/go/ls/grep/rg/find/make/docker/ps/du/df/stat/jq/ssh/golangci-lint…
   (`cat f` → `rtk read f`; the form `rtk rewrite '<cmd>'` prints; exact output and program code stay bare:
   `rules/standards/rtk.md`); Agent `description` is `<model>: lower-case task`.

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
| Test, lint, review | `go test ./internal/<pkg>` · `claude-code agents diff --check` · `/code-review` | `rules/standards/quality.md` |
