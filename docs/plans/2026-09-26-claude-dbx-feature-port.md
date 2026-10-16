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

## Round 2 (2026-10-16) — dbx is being retired; every fix lands here only

Context: the admins capped tokens on the Databricks workspaces, so `claude-dbx` is a read-only reference from now
on. Bugs found while porting are fixed in this repo, never there.

| # | Feature | Status / owner |
| --- | --- | --- |
| 8 | `docs-placement` hook | **done** — `internal/hooks/docsplacement.go`, registered, live-checked (root `.md` blocked, `docs/plans/` passes) |
| 4 | `rtk-wrap` hook + doctor line | **done** — `internal/hooks/rtk.go`, `HARNESS_RTK_BIN` override, `--porcelain`/value subcommands bypass |
| 11 | **Memory, native store**: MEMORY.md ≤20-line injected core, overflow pointers → `SEARCHABLE.md` (recall-only), archive/restore cold topics, `memory archive [--dupes\|--apply\|--restore\|--split N]` | in flight |
| 12 | **Memory, bank**: `memory doctor\|stats\|timeline\|promote`, caps → promote local→global → archive overflow (in `SettleBank`), `access.log` + hot/cold nominations, cached doctor notice at SessionStart | in flight |
| 13 | Agents (`claude`, `codex`, `copilot`, `documentation`, `reflection`), commands (`harness/*`, `reflection/*`, `workflow/route\|workflow`, `codex/*`, `memory/promote`), rules (`performance`, `workflow/*`, `harness/*` — all `paths:`-scoped) | in flight |
| 10a | `agent-effort` local classifier (unset spawn model → haiku/sonnet by task tier; roster `model:` pin wins) | queued |
| 10b | `clean` verb (dry-run, `--apply`, `--list`, `--older-than`) over this repo's own state dirs | queued |
| 10c | config `history` (shadow git of settings.json / providers.json / vendor configs: `snapshot\|log\|diff`) | queued |
| 14 | `mcp doctor` (connect health via `claude mcp list`, cross-scope drift, inline plaintext secrets) + `/harness:mcp-doctor` | queued |
| 15 | `theme` verb (pick the statusline palette; `--preview`) | queued |
| 16 | **Help rewrite** — the dbx menu exactly (`~/.claude/internal/cli/help.go` + the `Group:` field in `registry.go`: `usage:` header with the `<command> -h` line, UPPERCASE group headings, name column + one-line summary, hidden machine verbs), MINUS the GATEWAYS group (no databricks/apollo, and one anthropic entry is not a section). Groups: AGENTS (claude run ask agents gemini) · PROVIDERS (providers models use token router) · MEMORY (memory) · SESSION (clean history science usage review workflow) · DIAGNOSTICS (doctor) · SETTINGS (settings install generate configure theme). Every summary ≤60 chars so no row wraps at 80 cols; `hook`/`statusline`/`version` listed under the trailing "Machine helpers" note like dbx, not as rows; per-verb `-h` trimmed the same way. Runs LAST, once every verb above has landed | queued, last |
| 17 | **`generate` + `configure`** — see below | planned |

Skipped on purpose: `agent-rules-guard` (needs the per-agent native-memory RULES region + seeding, a subsystem this
repo does not have), dream/reflect codex passes, knowledge corpus, handoff beacons, ledger.

### 17 — `generate` and `configure`

Goal: one verb renders every config the harness owns, one verb walks a box through setup — Claude Code AND the
vendor agents (copilot, codex, gemini, opencode) on the same provider registry, so a provider/model chosen once
routes every CLI.

- `claude-code generate [--check] [--dry-run]` — renders `settings.json` (today: `settings --out` / part of `install`)
  PLUS each vendor's config from the registry, in one pass, no launch: codex `~/.codex/config.toml` provider block
  (`--write-codex-base`), opencode `~/.config/opencode/opencode.json` provider + imported Claude permissions/MCPs
  (the `run opencode` import already exists in `internal/vendors` — reuse it, do not re-implement), gemini
  `~/.gemini/settings.json` (API-key auth selection; Google login is reported, never overwritten), copilot
  (`GH_TOKEN`/`copilot login` → `github-copilot` passthrough provider; config is launch-time env, so `generate`
  only reports). Banner-marked blocks, foreign keys untouched, `.backup` once, `--check` exits non-zero on drift.
  `AGENTS.md`/`GEMINI.md` (`agents docs`) and the `~/.gemini/skills` mirror (`agents sync --gemini`) fold in.
- `claude-code configure [--provider p] [--dry-run] [--check] [<section>]` — the dialogue over existing verbs,
  dbx `configure` shape minus Databricks: **provider** (arrow-pick from the registry → `use`), **keys** (per
  provider, `auth.env` name → `env.d/secrets.env` 0600; a provider with a CLI login — `codex login`, `copilot
  login`, `gemini` Google auth — offers it first), **models** (`models --live` → `--pin`), **agents** (= `generate`
  for the vendors, then `run <vendor> --print-env` as the proof), **router** (start the unit when a cross-provider
  slot or `fallback` needs it), **doctor** (every check + the offered fix). Non-interactive when piped: report +
  the exact command for each repair, run CONTINUES so one pass reports the whole box. Browser logins are never
  run for the user.
- Order: `generate` first (pure rendering, testable byte-for-byte against fixtures), then `configure` on top of
  it. Both reuse `internal/vendors.Configure` (one wiring function per vendor) so a route written once serves
  `run`, `ask`, and the rendered config.

### Menu + verb parity decision (2026-10-16, owner)

The menu mirrors claude-dbx's exactly except GATEWAYS (gone) and `proxy` (not for now). Primary names follow dbx;
today's names stay as hidden aliases where a contract depends on them (`token` is the apiKeyHelper command).

| Group | Rows | Notes |
| --- | --- | --- |
| AGENTS | claude · copilot · codex · gemini · opencode | copilot/codex/opencode become top-level launch verbs (= `run <v>`) with an `ask` subverb (= `ask --agent <v>`) |
| AUTH | secrets · resolve · profile | `secrets` = file plane + OS keyring + scan/git/doctor audits (item 18); `resolve`→`token`, `profile`→`providers` |
| SESSION | switch · clean · history · science · memory | `switch`→`use` |
| DIAGNOSTICS | doctor · generate · usage · mcp | `generate`→`settings` today; grows into item 17's all-config render |
| SETTINGS | configure · install · theme | `configure` = item 17 |
| hidden | run · ask · agents · models · router · review · workflow · hook · statusline · version | dispatchable, named in the trailing note |

`configure` is the first-party auth front door: pick a provider, run that provider's own login (`codex login`,
`copilot login`, `gemini` Google auth) or paste a key into `env.d/secrets.env`, render every vendor config, prove
each with `--print-env`. Item 18 (`secrets`) lands before item 17.


### Auto-configure before an error (2026-10-16, owner)

A vendor verb (`claude-code codex|copilot|opencode|gemini`) never ends on a bare error while a human is there.
Order in `launch.Vendor`:

1. Resolve on the session provider; if it cannot serve the vendor, fall back to the vendor's own login
   provider when its login is present (codex → chatgpt, copilot → github-copilot, opencode → github-copilot
   via `opencode auth login` or the first keyed provider, gemini → GEMINI_API_KEY / Google login). Done.
2. If nothing resolves and stdin+stderr are a TTY: say why in one line, then offer `configure` for THAT vendor
   (`Set up codex now? [Y/n]`): detect an existing token/key/login and use it, else offer the vendor's own
   login (`codex login`, `copilot login`, `opencode auth login`, gemini auth) attached to the terminal, else
   prompt for a key into `env.d/secrets.env`; then retry the resolution once.
3. Only when piped, declined, or the retry still fails: the error line plus the exact command to fix it.
