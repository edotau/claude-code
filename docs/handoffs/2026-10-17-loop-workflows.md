# Handoff — loop workflows: recursive audit→fix→verify, vendor legs, agent trigger

Written 2026-10-17 by session `418826d1` (Fable 5.1). Pick up here; `activeContext.md` points at this file.

## Where things stand

All work is committed and pushed on `main` through `8b4b372`:

| Commit | What |
| --- | --- |
| `d2d53e8` `6a88787` | help footer dropped + README SVG regenerated; `clean`/`doctor` detect a nested `~/.claude/.claude/` |
| `0330956` `36e2227` | backups swept at any age; `defaultMode: auto`; fable `modelSettings` dropped (also hand-removed from live settings.json) |
| `0992438` | agent `test-repair` → `sonnet` (frontmatter `name`, every reference, `dist/` re-synced) |
| `8b4b372` | **Remote Control**: a login (`passthrough`) session no longer routes on the fallback ladder alone (`providers.Selection.NeedsRouter`); `claude --router` still forces it |

Remote Control was hidden because the shim set `ANTHROPIC_BASE_URL=http://127.0.0.1:18765/p/subscription`
(docs: unavailable with any non-`api.anthropic.com` base URL). Verified: `claude --print-env` now shows
`unset ANTHROPIC_BASE_URL`; `claude --router --print-env` sets it. **A fresh `claude` launch should list
`/remote-control`** (`remoteControlAtStartup: true` is already in the template). Cost: no Gemini failover for
subscription sessions unless `--router` is passed.

## The ultracode design run (this session, background)

`Workflow` run `wf_b5df47de-80b` — 4 readers (loops, agents, engine, vendors) → 3 proposals (minimal,
recursive, portfolio) → 3 judges → synthesis → completeness critic.

- Script: `projects/-Users-edotau--claude/418826d1-a406-46cd-99d2-5601aef66e01/workflows/scripts/loop-workflows-understand-design-wf_b5df47de-80b.js`
- Journal + per-agent transcripts: `projects/-Users-edotau--claude/418826d1-a406-46cd-99d2-5601aef66e01/subagents/workflows/wf_b5df47de-80b/journal.jsonl`
- `resumeFromRunId` is same-session only; a new session reads the journal (`"label":"synthesize"` and `"critic"` rows carry the plan).

**Goal it was given** (verbatim contract for the implementation pass):

1. `audit-fix-verify` / `implement-and-verify` / `gate-loop` become language-agnostic and runnable against ANY repo via
   `cwd` — gate auto-detected from the repo (Makefile targets, go.mod, pyproject, package.json), no `*.go`-only
   scope filters.
2. The loop recursively re-audits what it just fixed and reiterates until dry (K=2) or capped — no silent caps
   (`log()` every drop).
3. Vendor legs as stages: `claude-code ask --agent codex|gemini|copilot|opencode --format json` from a Bash step —
   codex adversarial verify, gemini 1M sweep for Scope/Find on big trees, copilot for the commit tail — each guarded
   by `claude-code agents` health with fail-open fallback to a Claude leaf. Known: codex `exec` hangs (ChatGPT
   limit), gemini is free-tier.
4. One harness agent (`claude`, plus `rules/workflow/master-workflow.md` and `skills/subagent-workflows`) is told WHEN
   to consider these workflows while developing a feature in another repo: MEDIUM+ change in a repo with a test
   target → `implement-and-verify`; post-refactor / pre-merge → `audit-fix-verify`.
5. **Cost-effective model per stage, enforced.** The design run above ran its 4 readers + 3 proposals on Fable 5.1
   (session tier) because `agent()` calls had no `model:` — `workflow lint` flags that only for saved `workflows/`,
   never for an inline ultracode `script`. Required: (a) every stage in the loop workflows pins the cheapest capable
   tier — scope/find/refute/judge/critic → `sonnet`, mechanical sweeps → `haiku`, fixers → `sonnet` with `opus` only
   for a confirmed high/critical, synthesis → `opus`; (b) the `agent-effort` hook (`internal/hookspec/hookspec.go:49`,
   `internal/hooks/agenteffort.go`) gains a `Workflow` matcher: run `internal/review/plan` lint over `tool_input.script`
   and rewrite unpinned `agent()` calls to `model: 'sonnet'` via `updatedInput` (fall back to blocking with the lint
   message if the rewrite is ambiguous); (c) `claude-code usage --workflows` reports per-stage model so the next audit
   can see the spend split. Test the block path, the rewrite path, and fail-open on an unparseable script.

## Synthesized plan — landed 2026-10-17 17:46

Full plan with exact line-anchored edits, verification steps and open questions:
**`docs/handoffs/2026-10-17-loop-workflows.plan.json`** (fields `plan`, `edits[]`, `verification[]`, `openQuestions[]`).
Critic gaps, when they land: `docs/handoffs/2026-10-17-loop-workflows.critic.json`. Winner: recursion-first + grafts.

Three parts, commit each as its own verified subset:

**A — cost (ship first).** `review-funnel.js:184-189` verifier is unpinned (`// lint:opus-ok`) → runs on Fable;
`implement-and-verify.js:76` pins `opus/high` unconditionally → `a.implModel || 'sonnet'`. `lint.go` gains
`LintPins(name, src []byte)` + pin VALUE validation (`model:'gpt-5'`/`effort:'med'` currently pass). New
`internal/hooks/workflowlint.go` + hookspec `{Name:"workflow-lint", Event:PreToolUse, Matcher:"Workflow"}` blocks an
inline `script`/`scriptPath` with an unpinned `agent()` (saved-by-name passes through; fail-open on read error).
Register in `hooks/io.go`; regen settings golden; `make install`.

**B — loops.** `gate-loop.js`: haiku Detect leaf (Makefile → go.mod → pyproject → package.json → Cargo.toml) only
when `args.gate` absent; `exit: green|stalled|capped|error`, returns `gate`/`gateSource`; stall check covers repeated
`error`; `fixesPerIteration` arg. `implement-and-verify.js` + `audit-fix-verify.js`: pass `gate: a.gate` through (no
synthesized Go commands), one static `TEST_GLOBS` list replaces `*_test.go`, multi-language `find`. `audit-fix-verify`
recurses via `workflow('audit-fix-verify', {paths: <touched non-test src>, round+1, prior})` with named exits
`dry|no-fix|stalled|nothing-changed|blocked|capped|error`, `unverified` bucket for null refuter batches, severity/package
normalization. New `workflows/vendor-leg.js`: haiku relay runs `claude-code ask --agent <leg> --format json --timeout ≤3m`
(smoke ask → envelope → sonnet fallback; never retries `--tools`; refuses >90 KiB). `audit-fix-verify {refuters:['codex']}`
adds codex as a second refuter per batch. Gemini/copilot/opencode reachable but not default stages.

**C — triggers/docs.** `CLAUDE.md:75` becomes a trigger row ("MEDIUM+ change in ANY repo → implement-and-verify;
before merge → audit-fix-verify; red gate → gate-loop") listing all 7 scripts; `:76` mentions the hook.
`agents/claude.md:154,194`, `rules/workflow/master-workflow.md:170,186,238-242` (+`dag`, `vendor-leg` rows),
`commands/workflow/orchestrate.md:61,64`, `skills/subagent-workflows/references/{choosing-a-pattern,cross-model-agent-teams}.md`.
Then `claude-code agents sync` and commit `dist/`.

**Finding worth knowing:** the dispatch log shows the **codex leg healthy today** (~5 s smoke asks, 2m24s audit) — the
"codex exec hangs" blocker in `activeContext.md`/`progress.md` is stale; correct both.

**Live checks the main session owns before any new test** (verification[] in the JSON): a scratch Python repo under
the scratchpad with a planted off-by-one → `audit-fix-verify {paths:['src'], cwd, rounds:2}` must find files, dispatch
a test agent, detect a pytest gate, produce round 2 and a named exit (if the host refuses same-name nested
`workflow()`, apply the for-loop fallback written into the audit-fix-verify edit); hook block/pass/pass-through;
gate detection on pyrepo, on this repo (`internal/review/plan` → NOT full `make test`), and on an empty dir → `error`.

**Not doing** (deliberate): no shell primitive, no confirm leaf after the fixer, no shared constants module, no
`dag`/`test-and-fix-fanout` rewrite, dead `RANGE || 'HEAD~1..HEAD'` left alone, no `usage --workflows` per-model split
(goal 5c stays open).

**Note on provenance:** the synthesizer's `plan` text opens by answering the Fable-routing question — the resumed run
inherited that user message as its "governing request" (known issue, `progress.md`). The plan content is unaffected.

**Critic gaps** (`…critic.json`, 7 edits — all citations verified against the tree): the two bank codex-line edits
are already done (skip); settle who owns the settings-golden regen + `make install` (Go lane vs test agent — one
owner, not both); decide whether `implement-and-verify` chains into `audit-fix-verify` so goal 2 holds for all three
loops (document either way in meta/Not-doing); add one demonstrated `vendor-leg` call per named role (gemini sweep,
copilot commit) or state they are undemonstrated; the `': error'` arm in gate-loop's new `exit` ternary is unreachable
— drop or comment it; the "3 retries × ≤3m fit in the Bash 10-min cap" rationale is wrong (ask never retries a
tools-on run; rewrite the sentence); have the implementer re-confirm the Workflow PreToolUse payload shape
(`tool_name == 'Workflow'`, `tool_input.script|scriptPath`) from a live hook capture before relying on it.

**Open questions it raised** (Eric decides before Part A/B land): keep Fable as the main-session model now that leaves
can't inherit it? hook = block (planned) vs auto-fill like `agent-effort`? keep `// lint:opus-ok` exempting a missing
`model:`? `rounds: 2` + `refuters: []` defaults (codex opt-in)? implementer `sonnet` with manual `implModel:'opus'` vs
auto-retry on `blocked`? wire gemini/copilot/opencode as default stages (no: quota/latency/--tools)?

## Where to run the next session

Start Claude Code in a **plain terminal hosted by the Orca UI** (not an Orca-spawned worktree worker): same risk as
any terminal, Remote Control works (direct launch), and Orca's multi-agent features can be tried one at a time from
there. Split the work: harness edits (`workflows/*.js`, hooks, lint) stay in Claude Code ultracode — their
verification surface IS the Workflow runtime; the recursive audit→fix→verify pass and the vendor legs are the
candidates for an Orca DAG (worker-per-worktree, decision gates, dashboard), first against a NON-harness repo.

**Rule 4 agent (add to the implementation fan-out, read-only, `sonnet`):** investigate and propose the rewrite of
CLAUDE.md hard rule 4 for worktrees. Must resolve: (a) the hazard is history-rewriting ops on the LIVE checkout; a
worktree is a separate checkout — but `claude-code` verbs resolve `paths.ConfigDir()` to `~/.claude` unless
`CLAUDE_CONFIG_DIR` points at the worktree (the Makefile already does `CLAUDE_CONFIG_DIR=$(CURDIR)`), and agent/rule
edits in a worktree are not live until merged; (b) `/workflow:worktree` + the `using-git-worktrees` skill create
`.claude/worktrees/<slug>` — in the harness repo that is `~/.claude/.claude/worktrees/`, which `clean`/`doctor`'s
nested-`.claude` check (`internal/clean/nested.go`) flags and `clean --apply` could remove: either exempt
`worktrees/` there or place harness worktrees elsewhere (Orca's own location) — decide and wire it; (c) merge-back is
fast-forward/merge into the live checkout, never `checkout`/`reset`; (d) output = the new rule text + the one-line
change to `internal/clean/nested.go` or `commands/workflow/worktree.md`, with a test for whichever moved.

## Next steps, in order

1. **Implement the plan** as an ultracode workflow — every `agent()` in that script pinned (`sonnet` implementers,
   `sonnet` test agent, `opus` only for a reviewer that must judge a design): implementers per file group
   (`workflows/*.js`, `agents/claude.md`, `rules/workflow/*.md`, `skills/subagent-workflows/*`,
   `internal/hooks/agenteffort.go` + `hookspec.go` for goal 5, `internal/review/plan/lint.go` only if lint must learn a
   rule) ‖ `sonnet` test agent in the same message → `gate-loop` with gate
   `claude-code workflow lint && make lint && go test ./internal/review/...`. Main session owns the live check:
   run `workflow("gate-loop", {cwd, scope, gate})` against a non-Go repo (e.g. `~/interview-prep`) before any new test.
2. **Recursive pass**: run `audit-fix-verify` over `main..HEAD` (the diff it just produced) — this is the loop
   auditing its own change; iterate until dry.
3. Vendor legs: wire only legs `claude-code agents` reports healthy; codex stays listed but fails open until its login
   is restored (`activeContext.md` → Blockers).
4. Commit per logical change (conventional, no AI footers), `make test` once at the end, push.

## Session notes (gotchas the next session should not re-learn)

- **Model inheritance in ultracode scripts.** The design run's first 7 agents ran on Fable 5.1 because the script
  omitted `model:`. Run stopped (`TaskStop`), script edited to pin the 5 unfinished stages (judges/critic `sonnet`,
  synthesis `opus`/`high`), resumed with `resumeFromRunId` — the 7 cached rows cost nothing to replay. Rule going
  forward: pin every `agent()`; `rules/workflow/agents.md` + `workflow lint` already say so for saved scripts.
- **Live-checking a launch plan.** `claude-code claude --print-env` is NOT a verb (prints "unknown command"); the
  real surface is the shim: `~/.claude/bin/claude --print-env` (and `--router --print-env`). An earlier "live check"
  in this session passed on that error until re-run through the shim.
- **Transient `PreToolUse:Bash hook error: ~/.claude/bin/claude-code: No such file or directory`.** Seen once this
  session; `go build -o` was measured NOT to leave a gap (0 missing samples over 5 rebuilds, atomic or not), and no
  harness code removes the binary — `make clean` from another session is the only known remover. Non-blocking; a
  PATH fallback would not help (`~/.local/bin/claude-code` symlinks the same file). Unresolved; low priority.
- `go clean -cache` was run by mistake while measuring the above; the first rebuild afterwards is just slower.
- `claude-code clean --apply` now sweeps backups at any age — each `make install` writes a fresh
  `settings.json.bak-<ts>`, so the next clean always lists one; that is expected.
- The `agent-effort` hook matcher is `Agent|Task` only (`hookspec.go:49`); Workflow `agent()` calls bypass it — goal 5.
- The `claude.md` agent's own rule already says "dispatch fan-out leaves as `code-workers`, never inherit" — the
  trigger wiring for goal 4 belongs next to that line (`agents/claude.md:154-162`), not in a new section.

## Open decisions for Eric

- Whether `audit-fix-verify` may fix lows (`minFix`) when run recursively — current floor is `medium`.
- Whether the copilot leg should ever commit inside a workflow or only stage and report.
- Keep `fallback: ["gemini"]` in the registry now that subscription ignores it, or drop it.
