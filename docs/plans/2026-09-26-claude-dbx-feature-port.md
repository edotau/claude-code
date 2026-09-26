# Port proven claude-dbx features into claude-code

Source: `~/claude-dbx` (Databricks harness). Target: this repo. Only provider-agnostic features; every
Databricks path (dbxauth, gateway, lanes/pools, UC, MLflow tracing, serving bench) stays behind.

## Standing goals (decided 2026-09-26)

- **Go, not Python.** Harness logic lives in the binary as a verb, never a `python3` script. The nine
  `skills/*/scripts/*.py` tools + `code-review-gate.sh` become `claude-code review <quality|complexity|assumptions|
  goals|diff|pr|report|gate>` and `claude-code workflow skeleton` (`internal/review/{source,plan,diff}`), checked
  byte-for-byte against the Python on real inputs, then the Python is deleted. Python complexity is an
  indentation estimate (no Python parser in Go); Go/JS/TS match exactly.
- **On PATH.** `make install` links `~/.local/bin/claude-code` (done, 0977f77); `~/.claude/bin` stays opt-in.
- **Mac only.** The WSL clipboard stack is out of scope: macOS Ctrl+V already pastes images and text. Revisit only
  if a macOS gap turns up (e.g. image paste into codex/copilot/gemini — new work, not a port).

## Ranked port list

| # | Feature | Source | LOC | Why |
| --- | --- | --- | --- | --- |
| 1 | `agent-inflight` — track and cap concurrent subagent dispatches | `hooks/inflight.go` | 283 | Guards the input-tokens/min cap fan-outs hit; many fix commits = mature |
| 2 | `simplify-after-edits` — SubagentStop gate: an editing subagent runs the simplicity skill once | `hooks/simplify_gate.go` | 112 | The skill exists here but nothing enforces it |
| 3 | `usage --workflows` — token/429 rollup per workflow phase from transcripts | `transcript/usage.go` + cmd | 207+ | Measures what the workflows actually cost |
| 4 | `rtk-wrap` — route noisy Bash through rtk | `hooks/rtk.go` | 188+100 | Token saving; no-op without rtk |
| 5 | VS Code Copilot Chat sync — mirror agents/skills/instructions for Copilot Chat (`agents sync --copilot`) | new, beside `internal/crossgen` | TBD | Pairs with the `github-copilot` login provider; verify VS Code's live read paths first |
| 6 | `save-plan` — copy an approved ExitPlanMode plan to `docs/plans/<date>-<title>.md` | `hooks/plan.go` | 96 | Small; matches this repo's docs/plans convention |
| 7 | Workflow refinements — refute batches, per-file diff chunks, `noTest` | `workflows/*.js` (684f0550, 8e9f8632, 59943c8b, 9abd1dcb) | diff | Rated large turn-count savings in dbx learnings |
| 8 | `docs-placement` — block markdown at repo root / in git-ignored dirs | `hooks/docsplacement.go` | 180 | Enforces rules/standards/documentation.md |
| 9 | `agent-rules-guard` — SubagentStop gate | `hooks/` (`AgentRulesGuard`) | ? | Read before deciding |
| 10 | `clean`, config `history`, `agent-effort` (local classifier only) | `internal/clean`, `settings/history*`, `hooks/agenteffort.go` | 380/565/240 | Useful, lower leverage |

Skipped: WSL clipboard stack (`scripts/wl-paste-claude.sh`, `internal/bmpfix`, `internal/clipboard`,
`hooks/promptpaths.go`, `launch/waylandenv.go`; ~820 LOC — Mac only), evidence/codex-capture (1.3k LOC, Databricks-routed codex), statusline spill (provider-specific),
TUI bracketed paste (no TUI here), knowledge corpus (overlaps `memory search`; compare before porting).

## Execution

One item per commit, in rank order. Each: port → `make install` → live check on the real surface (a real fan-out for
inflight) → `test-repair` dispatched with the port, ported tests run scoped to the
touched packages. `make test` once, after the last item.
