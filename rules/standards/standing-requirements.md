# Standing Requirements

Record owner requirements here or in the owning rule before implementing them. Once enforced, shorten to a pointer
naming the enforcer; remove prose only when enforcement makes it redundant.

- **R1 Plan ticks:** verified main commits tick named active-plan tasks after `verify:` and scoped gate pass; lane
  commits mark pending landing. Enforced by `docs-plan`, `worktree commit --task`, `plan tick --verify`.
- **R2 Dead tasks:** strike invalidated tasks with a cited decision (`plan task <id> --reason <r> --evidence <c>` or
  `DECISION: … strikes: <plan-slug> <id> (<reason>)`), never delete or leave open. Enforced by `plan task`, `docs-plan`.
- **R2 Unknown state:** strike with a comment; move to the backlog plan (do now) or techdebt.
- **R3 Techdebt:** one `~/.claude/memory/techdebt/<area>.md` per area.
- **R4 Plans audit:** audit plans under 7 days old. Enforced after `memory update` unlock and in `update --dream`.
- **R4 Boilerplate:** for prior-day plans, tick built process steps or strike when all build tasks are struck. Enforced
  by `plan tick --audit`.
- **R5 Verify returns:** follow CLAUDE.md dispatch guidance; rerun one claim before committing. Package tests: R32.
- **R5 Cheap authors:** fully read Haiku, Kimi and fallback-relay work; get higher-tier, other-family review.
- **R6 Model mix:** Sonnet authors; waves of 3+ put a third on other families (count resolved model, not CLI).
  Reviewer is another family (R5 applies). Rotate external agents by strength; GPT defaults to sol by alias.
  Job mapping: `skills/subagent-workflows/SKILL.md` → Leaf types.
- **R7 Databricks:** prefer overlay provider databricks rotating dev/qa/prod. Enforced by `agents ask` overlay
  `provider`.
- **R8 Typed model:** no silent swaps; unserved models fail with nearest ids; reports name the model. Enforced by
  `unservedError`; retry policy remains prose.
- **R9 Memory recall:** every brief carries recall. Enforced by `dispatch-effort` and ask's `withRecall`.
- **R10 File-blocked:** when 2+ tasks wait on one file, propose a lane; `worktree-git.md`.
- **R11 make:** leaf targets/checks follow `quality.md`: lint, scoped diff checks, audit/install/fmt/build/verify/
  bootstrap and fixing `lint-<kind>` in the harness tree.
- **R12 Dispatch timeout:** 15m default (up to 30m in plan task before dispatch; never extend mid-run; split larger
  tasks). At timeout parent finishes work without resending (R34); past Bash's 10m cap use `--background` and
  `agents wait`. Enforced by `DefaultToolsTimeout`.
- **R13 Guard walls:** `safety-scan` blocks deny matches and AI attribution (`hooks.md`). CLAUDE.md writes are
  main-session-only (leaves hand back the edit); lint/type suppressions ask in main, deny in agents.
- **R13 New walls:** new restrictions need the owner's yes; show permission additions as a table first.
- **R14 Transcripts:** never print secrets (byte counts only); `--secret` is main-only. Enforced by credential
  redaction and `secrets-out`/`secrets-end`.
- **R15 Plan tests:** default to basic feature tests (seconds); defer long tests to dream. Enforced in templates and
  dream Phase 5c.
- **R16 Test time:** reduce runtime without parallel tests or changed assertions. Leaves run only written/repaired
  tests by exact function; packages run once at sign-off, full suite once (at most twice) at landing. Aim <20s/pkg solo.
- **R17 Security reviews:** secrets/guard/SSH/sync/exfiltration reviews go to Gemini first. GPT briefs ask about
  robustness and data loss, not attacks, to avoid vendor-filter refusals.
- **R18 Plan first:** multi-part work starts with a dated `docs/templates/plan.md` plan before lanes or dispatch.
- **R19 Mac first:** build and live-check harness changes on Mac. WSL sync is periodic (session end or
  Linux/WSL-specific changes), never a push blocker.
- **R20 Databricks arm:** per-dispatch, never stored. Most leaves run Pi first (Claude in-context if refused) or
  carried Databricks asks rotating dev/qa/prod; unflagged agents keep their pins unless `COWORKER_OVERLAY` overrides.
- **R21 Live before tests:** after coding, live-check main, another repo, /tmp, a subagent and each external agent;
  writing new tests then waits for the owner's yes (the landing suite run needs none).
- **R22 Performance:** make the same work faster, removing no checks; profile and measure before/after for every
  feature. Profile/fix leaves on bench regressions, doctor hook p95 over budget or slow dispatch; rerun inconclusive
  loaded benchmarks. Enforced in part by post-commit bench and doctor; hook changes affect cost only (R27).
- **R23 Continuous ship:** green build/touched tests → push main → Mac install/live check; WSL follows R19. Verified
  lanes land locally uncommitted without asking; the owner commits (`CLAUDE.md`, `worktree-git.md`).
- **R24 Fast tier:** builds/gates, predetermined edits and renames/moves use `Tier: fast` and overlay `model:auto`
  (exclude Codex on Databricks); judgment stays standard/deep (R5). Enforced by `fastTask`.
- **R25 Fix as found:** fix bugs, diagnostics, dead code and stale docs now within the owner's priority; no asks or
  parked findings. Nonblocking out-of-scope finds go in one short list; report only owner judgments with
  recommendations (`quality.md` → Completion).
- **R26 Self-validation:** use the harness for real dispatches/reviews/audits/queries and fix what it finds (R25). No
  standing token-spending loops; `review bench --loop` needs owner approval per change's packages.
- **R27 Hook policy:** automated work on hooks, guards and permissions changes cost only, never allow/block policy.
  Seek what more can safely be allowed; tightening needs the owner's explicit request.
- **R28 Review loop:** collect finished lanes without lane reviews; run one other-family review of the merged diff and
  fix all findings without asking until none remain (stop after 3 rounds with a new HIGH in one area). Merge only with
  no CRITICAL/HIGH; never park findings. Enforced in `agents ask` briefs; `quality.md` → Completion.
- **R29 Broken tests:** main checks whether implementation meets the task, fixes minor test drift directly, and sends
  real defects to the author/repair leaf (exact test name, then main reruns touched packages). Never delete or skip
  tests; `quality.md` → Completion.
- **R30 No agent policy:** dispatched agents and memory jobs carry no permission policy; only `safety-scan` applies
  (`agent.md` → Dispatch behavior).
- **R31 Land on completion:** finished lanes land locally uncommitted or queue for main. Enforced by `lane-land`;
  `worktree-git.md` → Worktrees.
- **R33 Peer coordination:** message holders of needed files/plans/areas immediately (`ListAgents`, `SendMessage`),
  record holdings/landing times in the plan, work free areas, and re-check after their estimate. Never idle on peers
  or edit files they hold.
- **R34 Main takes over:** never resend budget-ended tasks (time/tokens/output/context); finish their files in main.
  After two failed attempts or broken dispatch, main does remaining work until dispatch recovers.
- **R35 Hook model cap:** automatic fill is capped at sonnet or equivalent/lower tier regardless of plan; announce
  tasks needing more to main. Other hook dispatches keep their models.
- **R36 Two schemas:** launch overlay (settings/vault/configs) and dispatch overlay (`--overlay {}`) only;
  endpoint/provider/profile travel together and children derive by copy. Third schemas, wrapper keys, combined forms
  or external config flags/env need owner approval.
- **R32 Dispatch workflow:** one lane per task; `worktree land` is the sign-off gate (one collector review per R28).
  Enforced by `land`/`lane-land`; `skills/subagent-workflows/SKILL.md` → Dispatch workflow.
