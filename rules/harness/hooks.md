---
paths:
  - "internal/hooks/**"
  - "internal/hookspec/**"
---
# Hooks System

Lifecycle hooks run natively as `claude-code hook <name>`, declared in `internal/hookspec/`
and implemented in `internal/hooks/`.

**The authoring surface is `internal/hookspec/hookspec.go`** — one `Spec` per hook invocation,
and the single place a hook is declared. To add, remove, or retime a hook: edit `hookspec.go` +
the Go handler in `internal/hooks/`, then `make install` to render the updated chain into
`settings.json`. **Never hand-edit `settings.json`** → `settings-management.md`.

## Event → hook matrix

| Hook | Event | What it does |
| --- | --- | --- |
| `safety` | PreToolUse (Bash) | Blocks destructive shapes: `rm -rf /`, force-push to main, `curl \| sh`, `sudo`, `DROP TABLE`, `git stash/checkout/reset/restore/clean` aimed at `~/.claude`, … |
| `rtk-wrap` | PreToolUse (Bash) | Routes noisy commands (`git`, `npm`, `cargo`, `kubectl`, `pytest`, `docker`) through the `rtk` output compressor when it's on PATH; machine-readable flags/value subcommands bypass it. `HARNESS_RTK_BIN` overrides the binary |
| `docs-placement` | PreToolUse (Write\|Edit) | Blocks a new `.md` at a repo root or under a git-ignored `docs/`, naming the canonical home. `HARNESS_DOCS_PLACEMENT=0` disables |
| `agent-effort` | PreToolUse (Agent) | Fills an UNSET subagent model from a local prompt classifier (low→haiku, else sonnet). No env override — an explicit `model:` on the dispatch or in the agent's frontmatter always wins |
| `context-checkpoint` | Stop | Once per session at ≥85% context, blocks Stop so the model can hand off (via `/harness:adapt`) before compaction |
| `stop-format` | Stop | Formats files edited this turn: the repo's own `make format\|fmt` target when it honors a per-file variable (`TARGET`/`FILE`/…), else the pyproject-declared python chain, else gofmt/ruff/prettier/shfmt when installed |
| `session-start` | SessionStart | Injects the local memory bank (char-capped); the global bank is read alongside it |
| `memory-recall` | UserPromptSubmit | Injects up to 3 bank/native-memory blocks matching ≥2 prompt terms, skipping any already in this session's context |
| `access-trace` | PostToolUse (Read) | Logs a model read of a bank file (or its index) to that bank's `access.log`, the hot/cold signal `memory doctor` nominates from; never blocks |
| `session-harvest` / `session-harvest-end` | Stop / SessionEnd | After enough edits, spawns a detached `claude-code memory update` that runs the equivalent of `/memory:end`. The first dispatch also claims one detached `memory watch` per session: once the transcript is quiet 10 min with uncovered edits it harvests the final state, so a crash or kill (no SessionEnd) loses nothing |

## Notes

- Hook exit codes and stdin/stdout follow the Claude Code hook contract; an async hook does not
  block the turn.
- Never use `--dangerously-skip-permissions`. Permission guardrails live in the template's
  `permissions` block (`ask` beats `allow`), regenerated into `settings.json`.
- Every blocking gate has a bounded fail-open cap (see `guardrails.md`) so a stuck gate can't
  trap a session.

## Related

- `settings-management.md` — the template is the source of truth for `settings.json`
- `guardrails.md` — the gate inventory and its evidence contract
