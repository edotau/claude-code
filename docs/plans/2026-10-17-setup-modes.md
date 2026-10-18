# Setup modes: adopt, nuke, workspace — plan

Date: 2026-10-17. Status: tabled; tasks drafted, nothing implemented. Follows the Tailscale phase 1 work.

## 1. Problem

The harness repo **is** `~/.claude`. On a fresh box `git clone` refuses a non-empty `~/.claude`, and on a box that
has run Claude Code (or an older rewrite of this harness) the directory already holds two kinds of content:

- **Runtime state Claude Code writes** — `projects/`, `sessions/`, `history.jsonl`, `backups/`, `plugins/`,
  `shell-snapshots/`, `file-history/`, `statsig/`, `cache/`, `debug/`, `ide/`, `todos/`, `policy-limits.json*`,
  `remote-settings.json*`, `.last-*`. Already gitignored at the repo root: never a clash, and must stay in place
  (transcripts, memory, plugin installs).
- **User-authored content at paths the repo tracks** — `CLAUDE.md`, `agents/*.md`, `commands/**`, `skills/*`,
  `rules/**`, `settings.json` (generated here, hand-written on a stock install), `keybindings.json`, plus
  `providers.json` / `env.d/` / `settings.local.json` which the repo leaves untracked but reads.

The 19 tracked top-level paths today: `.githooks .github .gitignore agents CLAUDE.md cmd commands docs env.d
go.mod go.sum internal LICENSE Makefile providers.example.json README.md rules skills theme workflows`.
The previous two rewrites were migrated by hand, directory by directory — the thing this plan replaces.

## 2. The three modes

| Mode | When | Keeps | Replaces |
| --- | --- | --- | --- |
| **adopt** (default) | `~/.claude` exists with state and maybe user content | all runtime state in place; user content at colliding tracked paths is backed up, not merged | the colliding tracked paths with the repo's |
| **nuke** | the directory is beyond repair, or a deliberate fresh start | nothing under `~/.claude`; `~/.claude.json` and the Claude Code CLI itself are separate decisions | everything: fresh CLI install + fresh clone + `bootstrap` |
| **workspace** | a repo or workspace wants its own `.claude/` | the global harness untouched | nothing; creates `<root>/.claude/` with project-scoped agents/commands/settings and a memory bank |

Recommendation: implement **adopt** first (it is the fresh-box path and the migration path), **nuke** as a
documented procedure plus one guarded verb, **workspace** last, after deciding what "project-scoped harness"
actually contains (§5).

## 3. Shared primitives (one package, `internal/setup`)

- `Clashes(repo, dir) ([]Clash, error)` — for every git-tracked path in the clone, report `exists-in-dir` and
  `differs` (content hash; a directory counts per file). Untracked and ignored paths are never clashes — an
  untracked skill dir or `settings.local.json` is the user's own (CLAUDE.md → Where things live).
- `Backup(dir, paths, label) (string, error)` — copies the clash set to `~/.claude/backups/<label>-<date>/<path>`
  preserving relative paths and modes. `backups/` is gitignored and already swept by `clean --apply`, so a backup
  has a known lifetime and `clean --list` shows it. `env.d/secrets.env` is never copied into a backup (the
  `history` verb has the same exclusion); a secret stays where it is.
- `Report(clashes)` — the table the user cherry-picks from: path, theirs vs repo (`same` / `differs` /
  `only-theirs`), backup location. Cherry-picking stays a **human** decision made with `diff`; no three-way merge
  logic. The clash set has been a handful of files every time (CLAUDE.md, a few agents/commands, settings.json).
- `Doctor` row — "setup: adopted <date> from <commit>; N backups pending review" sourced from
  `state/harness/setup.json`.

## 4. Tasks per mode

### 4.1 adopt

The binary does not exist before the clone, so adoption is a two-step bootstrap: clone to a staging path,
build, let the built binary adopt the live directory.

1. `scripts/bootstrap.sh` (POSIX sh, curl-able; one of the few shell files the repo keeps):
   `git clone <repo> ~/.claude.stage && make -C ~/.claude.stage compile && ~/.claude.stage/bin/claude-code
   adopt --from ~/.claude.stage --into ~/.claude`.
2. `claude-code adopt --from <clone> --into <dir> [--dry-run] [--label <s>]`:
   - refuse when `<dir>/.git` already exists (that is `git pull`, not adopt);
   - compute `Clashes`; print the `Report`; `--dry-run` stops here;
   - `Backup` the clash set; move `<clone>/.git` into `<dir>`; `git checkout -- .` restores every tracked path
     from the index (the user's versions are in the backup); remove `<clone>`;
   - write `state/harness/setup.json` {mode, date, commit, backup dir, clash count}; audit action `setup_adopt`;
   - finish with `make install` (settings.json regenerated through the existing `WriteWithBackup` merge, which
     already preserves hand-added permissions/hooks from the old file) and `configure --check`.
3. `settings.json` special case: a stock-install file is hand-written. `install` already merges it; the adopt
   report should call out what the merge kept (`settings.Summary`) so a dropped key is visible, not silent.
4. Skill symlinks: an existing `~/.agents/skills` link farm from an older harness is re-pointed by
   `agents sync`; adopt only reports it.
5. Tests: fixture tree in `t.TempDir()` with runtime dirs, a colliding `CLAUDE.md`, a colliding agent, an
   untracked skill; assert the backup holds exactly the two collisions, the skill is untouched, runtime dirs are
   untouched, and `--dry-run` writes nothing.

### 4.2 nuke

A fresh start has three independent layers; nuke must name which it is removing and ask per layer.

1. **Harness + state** (`~/.claude`): `claude-code nuke --dry-run` lists what goes (size per top-level dir,
   whether it holds untracked user content, whether `env.d/secrets.env` is present — secrets are printed by
   NAME only and the keyring is never touched). `--apply` requires typing the directory path back (not y/n:
   this is the one irreversible verb). Before deletion it writes one tarball of the **untracked user content
   only** (`providers.json`, `settings.local.json`, untracked skills/agents/commands, `env.d/provider.env`;
   never `secrets.env`) to `~/claude-nuke-<date>.tgz` outside the directory, then removes `~/.claude`.
2. **Claude Code account/CLI state** (`~/.claude.json`, outside the dir — `paths.ClaudeJSON()`): a separate
   `--account` flag; off by default. Removing it logs the user out and forgets MCP/project trust.
3. **The CLI itself** (`~/.local/bin/claude` → `~/.local/share/claude/versions/<v>` on this box): a separate
   `--cli` flag; off by default; reinstall is the official installer, documented in the README, not scripted by
   the harness (the installer's own script changes; the harness only verifies `claude --version` afterwards).
4. After nuke, the fresh path is §4.1 with an empty `~/.claude` (zero clashes) then `make bootstrap`
   (`TAILSCALE=1` optional), then `claude-code secrets set` for each name the tarball's manifest lists.
5. Tests: dry-run output against a fixture tree; `--apply` refused without the typed path; tarball excludes
   `secrets.env` and every gitignored runtime dir; `--account`/`--cli` never implied.

### 4.3 workspace

What Claude Code reads from a project `.claude/`: `settings.json` + `settings.local.json` (project scope),
`agents/`, `commands/`, `skills/`, and `CLAUDE.md` at the project root (or `.claude/CLAUDE.md`). The harness
already has per-repo pieces: `memory init` (bank under `docs/memory/bank/<repo-slug>/`), `clean` flagging a
nested `.claude/` **inside the harness checkout** as wrong, the `docs-placement` gate, and `/.claude/` in the
harness `.gitignore`.

1. `claude-code workspace init [<root>] [--with agents,commands,skills,settings,memory] [--dry-run]`:
   creates `<root>/.claude/` with a project `settings.json` carrying only project-scoped keys (permissions
   for the repo's tools, no hooks — hooks stay global), empty `agents/` `commands/` `skills/`, a starter
   `CLAUDE.md` at the root via the existing `/init` flow if none exists, and `memory init` for the bank.
   Refuses inside `~/.claude` itself (the nested-config check `clean` already has).
2. `workspace link <name>` — symlink a global agent/skill into the project `.claude/` so a repo pins a
   subset of the harness without copying; `workspace status` lists what is linked vs local vs shadowed
   (a project agent with the same `name:` as a global one shadows it — report it, as `agents sync` does for
   vendor renders).
3. `.gitignore` guidance for the target repo: `.claude/settings.local.json`, nothing else; the project
   `.claude/` is meant to be committed.
4. Tests: init into a temp repo; refuse inside the harness; link + shadow detection.

## 5. Open decisions

- **adopt**: back up the whole clash set (recommended) vs only files that differ (`same` files need no backup
  but the table is simpler when every collision has one).
- **nuke**: is a tarball of untracked user content enough, or should it also snapshot `projects/` transcripts
  and the memory banks (large; they are the thing most people regret losing)? Recommendation: `--keep-memory`
  copies `docs/memory/bank/` and `projects/*/memory/` into the tarball; transcripts are not kept.
- **workspace**: does a project `.claude/` get its own `providers.json`/router pin, or is provider choice always
  global? Recommendation: always global; a project pins a model at most (`HARNESS_<SLOT>_MODEL` in
  `.claude/settings.json` `env`).
- Verb naming: `adopt` / `nuke` / `workspace` as three verbs vs one `setup adopt|nuke|workspace`. Recommendation:
  `setup <mode>` — one row in the verb table, one package, one completion entry.
- Where `bootstrap.sh` lives and whether `make bootstrap` grows an `ADOPT=1` path that runs the same steps from
  an already-cloned staging dir.
- **Agent-driven adopt (later)**: when `claude` is already installed and logged in, `setup adopt --agent` could hand
  the clash report to a `claude -p` session that cherry-picks per file (auto, or asking the user per decision)
  instead of leaving the backup for a manual `diff`. Needs the headless credential rules from decisionLog
  2026-10-17 (setup-token only on `-p`). Not in phase 1.

## 6. Order

1. `internal/setup` primitives + `setup adopt` (+ `bootstrap.sh`).
2. `setup nuke` (dry-run first; `--apply` with the typed-path guard).
3. `setup workspace init|link|status`.
4. README → Setup section rewrite around the three modes; CLAUDE.md How-to row.
