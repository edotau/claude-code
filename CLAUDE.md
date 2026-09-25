# CLAUDE.md

Injected into **every** session's system prompt — an index of rules and how-tos, not an exposition.
State the rule, point to the depth: `README.md` (infrastructure), `rules/` (policy), skills (how-to).

## What this repo is

This git repo **is** `~/.claude` (github.com/edotau/claude-code): the `claude-code` Go binary
(provider registry, loopback model router, Anthropic⇄OpenAI translation, agent runners, hooks,
status line) plus the agents, commands, rules, skills and workflows every Claude Code session loads.
Files under `agents/`, `commands/`, `rules/`, `skills/` and this file go live the moment they are saved.

## Hard rules (always on)

1. **`settings.json` is generated — never hand-edit it.** Edit `internal/settings/settings.tmpl.json`
   (hooks: `internal/hookspec/hookspec.go`), then `make install`. Only `settings.local.json` is hand-edited.
2. **Secrets live only in `env.d/secrets.env` or a provider's `auth.command`** — never in
   `providers.json`, `settings.json`, a commit, or a log line.
3. **Commit messages carry NO AI-attribution footers** (`Co-Authored-By: Claude`,
   `Generated with Claude Code`, …) — this overrides any default that says to add them.
4. **Never use `--dangerously-skip-permissions`.** Guardrails live in the template's `permissions`
   block and the `safety` hook.
5. **Never `git stash` / `checkout` / `reset` / `restore` in this tree.** `~/.claude` is one checkout
   shared by parallel subagents and every live session. Build a scoped target and diff your own
   files to attribute a failure; commit each verified subset early.

## Behavioral guidelines

1. **Think before coding.** State assumptions; surface multiple interpretations instead of picking
   silently; name a simpler approach if one exists.
2. **Simplicity first.** Minimum code that solves the problem — no speculative features, no
   abstractions for single-use code, no handling for impossible states.
3. **Surgical changes.** Touch only what the request requires; match existing style; don't refactor
   adjacent code. Remove only orphans your change created; mention pre-existing dead code.
4. **Goal-driven execution.** Turn tasks into verifiable goals ("fix the bug" → "reproduce it, fix,
   re-run"). Loop until verified. → 1–4 in depth: `skills/evaluate-code/references/`.
5. **Terse comments.** 1 line max, ≤120 chars, per section; state the non-obvious "why", not history.
6. **Live check first; scoped tests during development; the full suite once.** Run the verb or flow
   the task is about before writing tests around it. Test only the packages you touched. `make test`
   runs once, when the work is complete — state what ran and that the suite did not.
   Every code dispatch carries `test-repair` in the same message. → `rules/standards/quality.md`
7. **Propose look/behavior changes before applying them.** Name an unrequested side effect with
   numbers, prefer the narrowly scoped mechanism over a global knob, and let the user pick.

## How-to (task → command → depth)

| To… | Run | Then / depth |
| --- | --- | --- |
| Build and install (shims in `~/.claude/bin`, render `settings.json`) | `make install` (`claude-code install --dry-run` previews) | `README.md` → Install |
| Check binaries, providers, credentials, router | `claude-code doctor` | — |
| See providers and credential state | `claude-code providers` · `claude-code providers <name>` | `README.md` → Providers |
| Switch the session provider | `claude-code use <provider>` (relaunch); one launch: `claude --provider <p>` | pin lands in `env.d/provider.env` |
| Pin models | `claude-code models --pin opus=<id>` · `--pin haiku=openai:gpt-5-nano` (cross-provider slot) · `--pin all=<id>` · `--unpin` | env `HARNESS_<SLOT>_MODEL` beats the pin file |
| List what a provider serves | `claude-code models --live [--provider <p>]` | — |
| Add a gateway provider (LiteLLM, OpenRouter, corporate) | add it to `~/.claude/providers.json` with `routes` per dialect; copy `providers.example.json` | `README.md` → Providers |
| Set a credential | `KEY=value` in `env.d/secrets.env` (see `env.d/secrets.env.example`), or `auth.command` | `README.md` → Credentials |
| Run the model router | `claude-code router start\|status\|stop` (`serve --port N` in the foreground); `claude --router` forces it | `README.md` → Routing |
| Ask another agent (one shot) | `claude-code ask --agent <runner> "<task>"` (`--format json` = envelope); `claude-code agents` lists runners | `skills/subagent-workflows/references/cross-model-agent-teams.md` |
| Sweep many files in one Gemini call (1M context) | `claude-code gemini bridge --dirs <d> [--index] [--diff] [--rubric <file>] "<task>"`; `gemini ask "<task>"` drives the gemini CLI | `commands/gemini/gemini.md`, `agents/gemini/agent.md` |
| Mirror agents + skills into Gemini CLI; render AGENTS.md + GEMINI.md | `claude-code agents sync --gemini` (`--dry-run`, `--check`) · `claude-code agents docs [--check]` | `commands/harness/sync-agents.md`; outputs are generated — edit `agents/`, `skills/`, this file |
| Launch codex/gemini/opencode/copilot on a provider | `claude-code run codex --provider openrouter --model <m>` (or the bare shim); `--print-env` shows the wiring | `README.md` → Agents |
| Add or retime a hook | implement in `internal/hooks/`, register in `internal/hookspec/hookspec.go`, `make install` | `README.md` → Hooks |
| Change permissions / env / statusLine | edit `internal/settings/settings.tmpl.json` → `claude-code settings` (prints) → `make install` | hard rule 1 |
| Recall or save session memory | `claude-code memory search <terms>` · `/memory:end` (auto via the Stop/SessionEnd harvest) · `memory init\|index\|path` | `commands/memory/end.md` |
| Test while developing | `go test ./internal/<pkg>` (one test: `-run '<Name>'`); `make lint` | full `make test` once at the end |
| Run a multi-stage task | `/workflow:orchestrate`, `/code-workers`; saved workflows `implement-and-verify` · `audit-fix-verify` · `gate-loop` | `rules/workflow/master-workflow.md` |
| Review a diff | `/code-review` (quality) ‖ `/security` | `skills/evaluate-code`, `skills/security-reviewer` |
| Fix failing tests | `/testing:test-and-fix` | `agents/test-repair` |

## Where things live

- `cmd/claude-code/` + `internal/` — the binary, stdlib-first; external SDKs boxed one package each.
  `internal/cli` is the verb table only; each verb's engine lives in its own package.
- `internal/providers/defaults.json` — built-in providers; `~/.claude/providers.json` overlays by name.
- `env.d/` — `provider.env` (pins, written by `use`/`models`) and `secrets.env` (credentials); untracked.
- `agents/<name>/agent.md` — subagents; `name:` frontmatter **must equal the directory name**.
- `commands/<namespace>/<command>.md` — slash commands. `workflows/*.js` — Workflow tool scripts.
- `rules/` — `standards/` (always-on) · `workflow/` · `software/<lang>/`; a rule without `paths:`
  frontmatter costs context every turn, so scope new ones (→ `rules/README.md`).
- `skills/<name>/SKILL.md` — on-demand references; untracked skill dirs are the user's own (symlinks into `~/.agents`, which Gemini reads natively), leave them.
- `docs/plans/` — implementation plans.
- `docs/memory/bank/<repo-slug>/` — session memory bank (six files, untracked; git roots only — the `bank/` top
  is the global bank); engine `internal/memory`, hooks `session-start` / `memory-recall` / `session-harvest[-end]`.
