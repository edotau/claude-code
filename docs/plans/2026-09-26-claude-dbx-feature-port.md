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
| 1 | **done 6fcf3a0** `agent-inflight` — track and cap concurrent subagent dispatches | `hooks/inflight.go` | 283 | Guards the input-tokens/min cap fan-outs hit; many fix commits = mature |
| 2 | **done** `simplify-after-edits` — SubagentStop gate: an editing subagent runs the simplicity pass once (reads `agent_transcript_path`; dbx reads the parent `transcript_path` — a dbx bug) | `hooks/simplify_gate.go` | 112 | The skill exists here but nothing enforces it |
| 3 | **done** `usage --workflows` — token rollup per workflow phase (dedups per-content-block usage lines; peak counts cache writes — both dbx bugs) | `transcript/usage.go` + cmd | 207+ | Measures what the workflows actually cost |
| 4 | `rtk-wrap` — route noisy Bash through rtk | `hooks/rtk.go` | 188+100 | Token saving; no-op without rtk |
| 5 | **done 5c3759b, no sync needed** — VS Code 1.139 reads `~/.claude/{skills,rules}` natively and `~/.claude/agents/*.md` once flat; agents flattened. Was: VS Code Copilot Chat sync — mirror agents/skills/instructions for Copilot Chat (`agents sync --copilot`) | new, beside `internal/crossgen` | TBD | Pairs with the `github-copilot` login provider; verify VS Code's live read paths first |
| 6 | **done** `save-plan` (reads `tool_input.plan`; dbx reads a nonexistent `planFile`; other repos' plans file under git-ignored `docs/plans/<repo>/`) — copy an approved ExitPlanMode plan to `docs/plans/<date>-<title>.md` | `hooks/plan.go` | 96 | Small; matches this repo's docs/plans convention |
| 7 | **done** (dbx's whole current audit-fix-verify; Fix keeps implementer ‖ test agent per quality.md) Workflow refinements — refute batches, per-file diff chunks, `noTest` | `workflows/*.js` (684f0550, 8e9f8632, 59943c8b, 9abd1dcb) | diff | Rated large turn-count savings in dbx learnings |
| 8 | `docs-placement` — block markdown at repo root / in git-ignored dirs | `hooks/docsplacement.go` | 180 | Enforces rules/standards/documentation.md |
| 9 | `agent-rules-guard` — SubagentStop gate | `hooks/` (`AgentRulesGuard`) | ? | Read before deciding |
| 10 | `clean`, config `history`, `agent-effort` (local classifier only) | `internal/clean`, `settings/history*`, `hooks/agenteffort.go` | 380/565/240 | Useful, lower leverage |

Skipped: WSL clipboard stack (`scripts/wl-paste-claude.sh`, `internal/bmpfix`, `internal/clipboard`,
`hooks/promptpaths.go`, `launch/waylandenv.go`; ~820 LOC — Mac only), evidence/codex-capture (1.3k LOC, Databricks-routed codex), statusline spill (provider-specific),
TUI bracketed paste (no TUI here), knowledge corpus (overlaps `memory search`; compare before porting).

## Performance, from the user's own Go (goFish, gonomics, gopher-proteinlab)

Baseline: `claude-code version` 7.1 ms, `statusline` 7.7 ms on a 5.8 MB transcript, `memory search` 11.2 ms —
process start dominates; nothing in these repos changes that. Patterns, not code, are what transfer.

| Status | Idea | Source | Target |
| --- | --- | --- | --- |
| done 6e48cfc | `ReadSlice` line scan with an overflow buffer | goFish `simpleio/simpleio.go:152` | `transcript.eachLine`: 14.6 → 2.2 MB, 6.5k → 2.1k allocs per Stop |
| in review port | Byte state machine + keyword switch, indent stack, SonarSource boolean-sequence rule | goFish `bam/cigar.go:87`, proteinlab `annotation/genbank.go:50` | Python complexity estimate in `internal/review/source` |
| next | k-gram seed index (`map[uint64][]pos`) → verify | gonomics `genomeGraph/index.go:21` (user-authored as `simpleGraph/`) | moved-block detection in `review diff`; near-duplicate check in memory harvest; trigram fallback for BM25 typos |
| next | Ordered worker-pool fan-out (fix `heapConcur.go:52` Peek bug; likely derived from tejzpr/ordered-concurrently — keep attribution) | goFish `dataflow/heapConcur.go:28` | multi-file `review` runs |
| later | Lazy regex (`sync.OnceValue`) for package-level `MustCompile`s | — | `agent/dispatch.go:77`, `vendors.go:133,444`, `translate/request.go:103`, `models.go:29`, `hooks/format.go:34` (<0.5 ms total) |

Not ported: pgzip/gzip readers (no compressed input), goFish BWT/sorts (`slices.Sort` wins), interval tree / external
merge sort (data too small), bwaGoFish (10X Genomics copyright — never copy). gonomics is BSD-3 multi-author:
keep the notice on anything copied.

## Execution

One item per commit, in rank order. Each: port → `make install` → live check on the real surface (a real fan-out for
inflight) → `test-repair` dispatched with the port, ported tests run scoped to the
touched packages. `make test` once, after the last item.
