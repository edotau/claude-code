# Standing Requirements

Record owner requirements here or in the owning rule before implementing them. Once enforced, shorten to a pointer
naming the enforcer; remove prose only when enforcement makes it redundant.

- **R1 Plan ticks:** verified commits automatically tick named active-plan tasks on main after their `verify:`
  and scoped gate pass on that sha. Lane commits mark pending landing; coverage only suggests once.
  No matching open task changes nothing; an unknown task is a note, not an error.
  Enforced by `docs-plan`, `worktree commit --task`, `plan tick --verify`.
- **R2 Dead tasks:** strike tasks invalidated by a decision, citing it; never delete or leave them open.
  `plan task <id> --reason <r> --evidence <c>`, or a recorded `DECISION:` ending
  `strikes: <plan-slug> <id> (<reason>)`, targets that exact id, never matching words.
  Enforced by `plan task` and `docs-plan`.
- **R2 Unknown state:** strike with a comment; move to the backlog plan (do now) or techdebt.
- **R3 Techdebt:** one `~/.claude/memory/techdebt/<area>.md` per area.
- **R4 Plans audit:** audit plans under 7 days old around memory update and in `memory update --dream`.
  Enforced after update unlock and by the `update --dream` plan phase with Go rechecks.
- **R4 Boilerplate:** for plans before today, tick process steps if built on origin/main; strike if all build tasks
  are struck; otherwise report. Enforced by `plan tick --audit`.
- **R5 Verify returns:** follow CLAUDE.md dispatch guidance; rerun one claim before committing. Package tests: R32.
- **R5 Cheap authors:** fully read Haiku, Kimi and fallback-relay work; get higher-tier, other-family review.
- **R6 Model mix:** Sonnet authors; waves of 3+ put a third on other families. Count resolved model, not CLI:
  Pi-first/rotated leaves count only when their model is non-Claude. Reviewer is another family; R5 still applies.
  Rotate real work through every external agent by strength; GPT defaults to sol by alias.
  Job mapping: `skills/subagent-workflows/SKILL.md` → Leaf types.
- **R7 Databricks:** prefer overlay provider databricks with profile, rotating dev/qa/prod.
  Enforced through `agents ask` overlay `provider`.
- **R8 Typed model:** no silent swaps; unserved models fail with nearest ids; reports name the model.
  Enforced by `unservedError`; retry policy remains prose.
- **R9 Memory recall:** every brief carries recall. Enforced by `dispatch-effort` and ask's `withRecall`.
- **R10 File-blocked:** when 2+ tasks wait on one file, propose a lane; `worktree-git.md`.
- **R11 make:** leaf targets/checks follow `quality.md`: lint, scoped diff checks, audit/install/fmt/build/verify/
  bootstrap and fixing `lint-<kind>` in the harness tree.
- **R12 Dispatch timeout:** every ask/submit gets 15 minutes. A parent may set overlay timeout up to 30 minutes
  only when the plan task states it before dispatch; never extend mid-run. Split tasks needing more.
  At timeout the parent finishes the existing work, never retries/resends (R34).
  Past Bash's 10-minute cap, use background jobs and `agents wait`. Enforced by `DefaultToolsTimeout`.
- **R13 Guard walls:** every agent's `safety-scan` blocks settings deny matches and AI attribution in commits,
  merges, tags or PRs. CLAUDE.md writes are main-session-only; leaves hand back the edit.
  Added lint/type suppressions ask in main (rare exception), deny in agents. Details: `hooks.md`.
- **R13 New walls:** new restrictions need the owner's yes; show permission additions as a table first.
- **R14 Transcripts:** never print secrets in sessions, reports or briefs; byte counts only.
  `--secret` is main-only; leaves receive credentials at launch.
  Enforced by credential redaction and `secrets-out`/`secrets-end`.
- **R15 Plan tests:** default to basic feature tests taking a couple of seconds; defer long tests until use warrants
  them. Dream lists further suggestions. Enforced in plan/brief templates and dream Phase 5c.
- **R16 Test time:** reduce runtime without parallel tests or changed assertions. Leaves run only tests they wrote
  or repair, by exact function, never the suite. Packages run once at sign-off; the full suite once, at most twice,
  at landing.
  Aim below 20 seconds per package solo; memory code/test speed is standing work.
- **R17 Security reviews:** secrets/guard/SSH/sync/exfiltration reviews go to Gemini first.
  GPT briefs ask about robustness and data loss, not attacks, to avoid vendor-filter refusals.
- **R18 Plan first:** multi-part work starts with a dated `docs/templates/plan.md` plan before lanes or dispatch.
- **R19 Mac first:** build and live-check harness changes on Mac. WSL sync is periodic (session end or
  Linux/WSL-specific changes), never a push blocker.
- **R20 Databricks arm:** per-dispatch, never stored. Most leaves run Pi first (Claude in-context if refused)
  or carried Databricks asks rotating dev/qa/prod, unless the owner specifies another route.
  Unflagged agents keep their pins unless explicit `COWORKER_OVERLAY` overrides.
- **R21 Live before tests:** after coding, live-check main, another repo, /tmp, a subagent and each external agent;
  writing new tests then waits for the owner's yes (the landing suite run needs none).
- **R22 Performance:** make the same work faster, removing no checks; profile and measure before/after for every
  feature. Real-use triggers get profile/fix leaves: touched-package bench regression, doctor hook p95 over budget,
  slow dispatch steps. Inconclusive loaded benchmarks get a rerun command, not a leaf.
  Enforced in part by post-commit bench and doctor; hook changes affect cost only (R27).
- **R23 Continuous ship:** green build/touched tests → push main → Mac install/live check; WSL follows R19.
  Verified lanes land locally uncommitted without asking; the owner commits. Added restrictions wait for approval.
  Authorization boundary: CLAUDE.md and `worktree-git.md`.
- **R24 Fast tier:** builds/gates, predetermined edits, renames/moves (including Pi-first) use `Tier: fast`
  and overlay `model:auto` to rotate the fast pool; exclude Codex on Databricks (no fast model).
  Judgment stays standard/deep; R5 applies. `fastTask` classifies untyped briefs.
- **R25 Fix as found:** fix bugs, diagnostics, dead code and stale docs now within the owner's priority; no asks
  or parked findings. Nonblocking out-of-scope finds go in one short list, not a new wave.
  Report only owner judgments (deleting parked work, new walls, behavior changes) with recommendations.
  See `quality.md` → Completion.
- **R26 Self-validation:** use the harness for real dispatches/reviews/audits/queries and fix what it finds through
  the harness (R25). No standing token-spending loops; `review bench --loop` needs owner approval for one change's
  packages.
- **R27 Hook policy:** automated work on hooks, guards and permissions changes cost only, never allow/block policy.
  Seek what more can safely be allowed; tightening needs the owner's explicit request.
- **R28 Review loop:** collect finished lanes on one branch; no lane reviews. One shared other-family review
  lists every finding; fix all without asking and repeat until none. Reviewers may edit the collector and list
  edits; author/main still reads their diff. Three consecutive rounds finding a new HIGH in one area stop the loop:
  bring the pattern and a design recommendation to the owner. Merge only with no CRITICAL/HIGH; fix remaining
  findings, never park them. Enforced in `agents ask` briefs; `quality.md` → Completion.
- **R29 Broken tests:** main first checks whether the implementation meets the task. Fix minor fixtures, expectations,
  hashes, rows or lane interactions directly; send real defects with failures to the author/new repair leaf.
  That leaf runs the repaired test by exact name; main then reruns all packages touched by repair/failing lanes.
  Never delete or skip tests. See `quality.md` → Completion.
- **R30 No agent policy:** dispatched agents and memory jobs carry no `--permissions`, inherited or per-agent policy;
  only `safety-scan` applies. See `agent.md` → Dispatch behavior.
- **R31 Land on completion:** finished lanes land locally uncommitted or queue for main.
  Enforced by `lane-land`; `worktree-git.md` → Worktrees.
- **R33 Peer coordination:** message holders of needed files/plans/areas immediately (`ListAgents`, `SendMessage`):
  state your work, ask holdings/collisions/landing time, record answers in the plan and work free/read-only areas.
  Ask again after their estimate; offer review and send your diff for their areas.
  Never idle on peers or edit files they hold.
- **R34 Main takes over:** never resend budget-ended tasks (time/tokens/output/context); finish their files in main,
  or restart that task there. After two failed attempts, or broken dispatch, main does remaining work until dispatch
  recovers. This overrides the rule sending work larger than one line to leaves.
- **R35 Hook model cap:** only automatic fill is capped at hardcoded sonnet, an equivalent tier or lower,
  regardless of the plan. Announce tasks needing more to main; other hook dispatches keep their models.
- **R36 Two schemas:** launch overlay (settings/vault/configs) and dispatch overlay (`--overlay {}`) only.
  Endpoint/provider/profile travel together. Derive a child by one function copying/changing the parent overlay,
  never a reference or ambient variable. Third schemas, wrapper keys, combined forms or external config flags/env
  need owner approval.
- **R32 Dispatch workflow:** one lane per task; `worktree land` is the sign-off gate, never a leaf gate chain.
  Tests at sign-off except a leaf's written/repaired test; one collector review (R28).
  Pi/external agents implement/fix; Claude verifies/commits in lane; main lands.
  The Opus owner fans out `agents ask --brief` contracts.
  Enforced by land/lane-land; `skills/subagent-workflows/SKILL.md` → Dispatch workflow.
