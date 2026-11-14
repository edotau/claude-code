# Wave reader fixture

## Phases

Every wave commit runs this gate (`$BASE` = the commit before the wave, `$OWNED` = the wave's owned files):

```bash
set -o pipefail
cd /Users/edotau/.claude
go build ./... && GOOS=windows go vet ./... && make lint
claude-code agents diff --base "$BASE" $(printf -- '--path %s ' $OWNED) --check   # exit 0; 3 = file outside
```



- **1.1 F-open, keyed readers.** Owns `internal/proc/keyed.go`, `internal/providers/*.go`, `internal/agentconfig/*.go`,
  `internal/router/*.go`, `internal/network/egress/*.go`. Dep D0. codex. Adds the `Context` readers of the Interface;
  old exports untouched. Verify `verify ./internal/proc ./internal/providers ./internal/agentconfig ./internal/router
  ./internal/network/egress` → two contexts pick distinct confined routes; an empty value suppresses ambient env; a
  request-pinned egress host (also explicit direct `""`) beats the run value.

## Waves

| Wave | Packages | Width | Depends on | Width bound / what widens it | Verify → proof |
| --- | --- | --- | --- | --- | --- |
| 1 add-only | 1.1–1.10 | 10 | D0 | dependency: all else consumes F and P1 / split F (adds 2 waves) | gate exit 0; Phase 0 diff = new verbs only |
| 2 enforce + runtime | 2.1–2.17 | 17 | wave 1, O1 O2 O4 O5 O6 | settings adapters need R / split R into launch-env and record-job (+1 leaf, adapters to wave 2) | `agents diff --verify` per leaf exit 0 |
| 3 consumers | 3.1–3.8 | 8 | wave 2 | migration barrier; adapters' second pass / merge P2–P6 here (narrower) | barrier `rg` clean |
| 4 close + prove | 4.1–4.7 | 7 | wave 3, D1–D6, R3, R5 | deletions need the barrier / none useful | nine `rg` exit 1; doctor exit 0 |

Totals: 38 packages, 42 package dispatches (F, R, D, H return in wave 4), widths 10 / 17 / 8 / 7, peak 17 leaves in
flight (test leaves run after their wave's implementers, never beside them), plus one verifier per wave.

Collisions resolved (17): **merged 3** — toolbox W0 + result-save + wait into 1.4 (all need `cli/agents.go`); brief
wiring + diff verify into 2.17 (`cli/agents.go`, wave 2); permissions P12's `rules/harness/agent.md` into the R5 trim
4.7. **Moved 4** — P7's memory-helper switch to 3.7 (intra-wave dependency on P10); P12 docs and P13 doctor to wave 4
(need merged behavior, settings adapters change launches); C4's `effort.go` edit to wave 1 (P11 wave 2, H wave 3).
**Separated or narrowed 10** — P2–P6 vs C/X/K/J/O (waves 2 vs 3); P7 and P8 vs D; P11 vs H; P1 vs R on `run.go`
(waves 1 vs 2); R, I and M narrowed to named files; E1's `record.go` in wave 1 ahead of R.

- [ ] Phase 0.1 — Capture the baseline — leaf: claude, Agent: in-context — owns: /tmp/pst
- [ ] Phase 1.1 — F-open keyed readers — leaf: codex, Agent: codex — owns: internal/proc/keyed.go
- [ ] Phase 1.2 — V record clone — leaf: opencode, Agent: opencode — owns: internal/vault/agentrecord.go
- [ ] Phase 1.3 — P1 permission contract — leaf: codex, Agent: codex — owns: internal/agent/permissions.go,
  definition.go, run.go, permission_contract.go
- [ ] Phase 1.4 — T1 wait, result --save — leaf: opencode, Agent: opencode — owns: internal/cli/agents.go,
  internal/agent/wait.go, internal/agent/ask/resultsave.go
- [ ] Phase 1.5 — T2 brief renderer — leaf: codex, Agent: codex — owns: internal/agent/brief.go, brief.tmpl
- [ ] Phase 1.6 — T5 cap check — leaf: opencode, Agent: opencode — owns: internal/review/diff/scope.go
- [ ] Phase 1.7 — T6 adoption rule — leaf: claude, Agent: claude — owns: rules/workflows/agents.md
- [ ] Phase 1.8 — C2 skip automated deliveries — leaf: codex, Agent: codex — owns: internal/hooks/core/input.go,
  internal/hooks/memhook/memory.go, internal/hooks/docs/promptpaths.go
- [ ] Phase 1.9 — C4 no repeated recall pointers — leaf: codex, Agent: codex — owns: internal/memory/recall.go,
  internal/hooks/agents/effort.go
- [ ] Phase 1.10 — E1 envelope fold — leaf: opencode, Agent: opencode — owns: internal/agent/record.go,
  internal/agent/tree/reap.go
- [ ] Phase 1.V — Verify wave 1, commit — leaf: claude, Agent: in-context — owns: /tmp/pst/w1
- [ ] Phase 2.1 — P2 claude enforcement — leaf: codex, Agent: codex — owns: internal/claude/gate.go, claude.go
- [ ] Phase 2.2 — P3 codex enforcement — leaf: opencode, Agent: opencode — owns: internal/codex/permissions.go,
  launch.go, codex.go
- [ ] Phase 2.3 — P4 copilot enforcement — leaf: codex, Agent: codex — owns: internal/copilot/sdk_permissions.go, sdk.go
- [ ] Phase 2.4 — P5 gemini enforcement — leaf: opencode, Agent: opencode — owns: internal/gemini/settings.go, gemini.go
- [ ] Phase 2.5 — P6 opencode enforcement — leaf: codex, Agent: codex — owns: internal/opencode/permission.go,
  opencode.go, plugin.go
- [ ] Phase 2.6 — P7 ask lifecycle — leaf: codex, Agent: codex — owns: internal/agent/ask/ask.go, batch.go, jobs.go,
  dispatch.go
- [ ] Phase 2.7 — P8 roles — leaf: opencode, Agent: opencode — owns: internal/agent/roles/run.go
- [ ] Phase 2.8 — P9 workflow stages — leaf: codex, Agent: codex — owns: workflows/code-analyzer.js, code-engineer.js,
  code-evaluator.js, code-workers.js, green-keeper-loop.js, internal/hooks/quality/workflowlint.go
- [ ] Phase 2.9 — P10 memory job scope — leaf: opencode, Agent: opencode — owns: internal/memory/update.go, jobscope.go
- [ ] Phase 2.10 — P11 hook denials — leaf: codex, Agent: codex — owns: internal/hooks/safety/permission.go,
  internal/hooks/agents/effort.go, leaves.go, codex.go
- [ ] Phase 2.11 — R runtime — leaf: codex, Agent: codex — owns: internal/agent/account.go, cli.go, engine.go, env.go,
  fallback.go, hostguard.go, job.go, launch.go, launcher.go, preflight.go, record.go, roster.go, route.go, run.go,
  settings.go
- [ ] Phase 2.12 — M auxiliary consumers — leaf: opencode, Agent: opencode — owns: internal/catalog/catalog.go,
  internal/databricks/databricks.go, internal/memory/embed.go, verbs.go
- [ ] Phase 2.13 — I CLI entry — leaf: opencode, Agent: opencode — owns: internal/cli/table.go, gateway.go
- [ ] Phase 2.14 — U menu, status line — leaf: opencode, Agent: opencode — owns: internal/ui/menu/configure_page.go,
  models_pick_page.go, internal/ui/statusline/statusline.go
- [ ] Phase 2.15 — G gateway — leaf: codex, Agent: codex — owns: internal/gateway/daemon.go
- [ ] Phase 2.16 — PF preflight — leaf: opencode, Agent: opencode — owns: internal/preflight/pins.go
- [ ] Phase 2.17 — T3 brief verb, owned verify — leaf: opencode, Agent: opencode — owns: internal/agent/diff.go,
  internal/cli/agents.go
- [ ] Phase 2.V — Verify wave 2, commit — leaf: claude, Agent: in-context — owns: /tmp/pst/w2
- [ ] Phase 3.1 — Q configure — leaf: opencode, Agent: opencode — owns: internal/configure/keys.go
- [ ] Phase 3.2 — C claude values — leaf: codex, Agent: codex — owns: internal/claude/launch.go, claude.go
- [ ] Phase 3.3 — X codex values — leaf: opencode, Agent: opencode — owns: internal/codex/codex.go, launch.go
- [ ] Phase 3.4 — K copilot values — leaf: codex, Agent: codex — owns: internal/copilot/copilot.go, sdk.go, route.go
- [ ] Phase 3.5 — J gemini values — leaf: opencode, Agent: opencode — owns: internal/gemini/gemini.go, settings.go
- [ ] Phase 3.6 — O opencode values — leaf: codex, Agent: codex — owns: internal/opencode/opencode.go
- [ ] Phase 3.7 — D ask and roles — leaf: codex, Agent: codex — owns: internal/agent/ask/armretry.go, coworker.go,
  ask.go, internal/agent/roles/run.go
- [ ] Phase 3.8 — H prediction hooks — leaf: opencode, Agent: opencode — owns: internal/hooks/agents/effort.go,
  leaves.go
- [ ] Phase 3.V — Verify wave 3, barrier, commit — leaf: claude, Agent: in-context — owns: /tmp/pst/w3
- [ ] Phase 4.1 — F-close — leaf: codex, Agent: codex — owns: internal/providers/pins.go
- [ ] Phase 4.2 — R-close — leaf: opencode, Agent: opencode — owns: internal/agent/launch.go
- [ ] Phase 4.3 — D-close — leaf: codex, Agent: codex — owns: internal/agent/ask/armretry.go, coworker.go
- [ ] Phase 4.4 — H-close — leaf: opencode, Agent: opencode — owns: internal/hooks/agents/effort.go
- [ ] Phase 4.5 — P13 launch-aware doctor — leaf: codex, Agent: codex — owns: internal/doctor/permissions.go
- [ ] Phase 4.6 — P12a docs — leaf: claude, Agent: claude — owns: agents/claude.md, codex.md, copilot.md, gemini.md,
  opencode.md, Plan.md
- [ ] Phase 4.7 — P12b rules — leaf: claude, Agent: claude — owns: rules/harness/agent.md, hooks.md, go.md,
  providers.md
- [ ] Phase 4.V — Verify wave 4, commit — leaf: claude, Agent: in-context — owns: /tmp/pst/w4
- [ ] Live check on the real surface (one denial → replay run per agent)
- [ ] Scoped tests for the touched packages
- [ ] Review (`claude-code codex review --adversarial --base <wave-1 base>`)
- [ ] Full suite once
- [ ] Commit, `make install`, push per wave (named files only)
- [ ] Handoff / `claude-code memory update`
