# Standing Requirements

A new standing requirement from the owner is written here, or in the rule that already owns its topic, before the code
that enforces it. When a hook or verb enforces it, the row shrinks to a pointer and "Enforced by" names the tool.
A row is deleted only when the tool makes the prose redundant.

| Requirement | Rule | Enforced by |
| --- | --- | --- |
| R1 Plan ticks | Tick on main when the commit names the task with `worktree commit --task` (any owns: coverage); coverage alone prints a candidate once | `docs-plan-tick` hook |
| R2 Dead tasks | Strike, never delete or leave open: `plan task <id> --strike "<why>"` | `plan task` verb |
| R2 Unknown state | Strike with a comment; move to the backlog plan (do now) or techdebt | prose |
| R3 Techdebt | Lives in `~/.claude/memory/techdebt/<area>.md`, one file per area | prose (until memory knows it) |
| R4 Plans audit | Before `memory update` and in `dream`: audit plans under 7 days old | `memory update` after unlock (deterministic audit); `dream` plan phase, re-checked in Go |
| R4 Boilerplate | Plan before today: tick process steps if built on `origin/main`, strike if all build tasks struck, else report | `plan tick --audit` |
| R5 Verify returns | CLAUDE.md guideline 3, plus re-run one claim and the whole suite before a commit | prose |
| R5 Cheap authors | Haiku, Kimi, fell-back relay: full read plus higher-tier, other-family review | prose |
| R6 Model mix | Sonnet authors; a wave of 3+ puts a third on other families; reviewer is another family | prose |
| R7 Databricks | Prefer `--provider databricks` over a fixed `--profile`; if named, rotate | `agents ask --provider` |
| R8 Typed model | No silent swap; unserved fails with nearest ids; report names model | `unservedError`; retry prose |
| R9 Memory recall | `hooks.md` → `dispatch-effort`: every dispatched brief carries the recall block | `withRecall` in `agents ask`; `--safe-mode` off |
| R10 File-blocked | `worktree-git.md`; 2+ tasks wait on a file: propose a lane | prose (plan worktree-lanes) |
| R11 make | `quality.md`; leaves: `make lint`, `agents diff --check`; no fmt, audit, install | `safety-scan` for main and undispatched agents; leaves pass `agentBashAllow` |
| R12 Dispatch timeout | Tools dispatch gets 15 minutes; a longer job passes `--timeout` | `DefaultToolsTimeout` |
| R13 Guard walls | `--secret`, `vault get --key --secret`, production exec/delete, `oc rsh`: `hooks.md` main-session rules | `safety-scan` for main (unless a settings allow covers it); leaves pass `agentBashAllow` |
| R13 New walls | A new guard wall needs the owner's yes; permission additions shown as a table first | prose |
| R14 Transcripts | Never print a secret value into a session, report or brief; byte counts only; `--secret` main session only, leaves get credentials at launch | credential verbs redact; hooks `secrets-out` (Bash output) and `secrets-end` (ended transcripts) |
| R15 Plan tests | Default, not absolute: new feature: basic tests that it works, a couple of seconds to run at most; no long tests before it has been used a while; more tests are suggestions `dream` lists | plan template, `brief.tmpl`, dream Phase 5c |
| R16 Test time | Keep cutting test run time, never by running tests in parallel; a leaf runs its package or exact test functions (`go test -run '^TestX$' ./internal/<pkg>`), never `./...`; the whole suite runs once before a push; aim as work continues: each package under 20 s solo | prose |
| R17 Security reviews | A review of secrets, guard, ssh, sync or exfiltration code goes to Gemini first: GPT's vendor filter refuses such briefs mid-run; a brief sent to GPT asks for robustness and data loss, not attacks | prose |
| R18 Plan first | Multi-part work starts as a dated plan (`docs/templates/plan.md`) before any lane or dispatch | prose |
| R19 WSL first | A harness change is built and live-checked on WSL first, then pulled and installed on the Mac | prose |
| R20 Databricks arm | Subagents and dispatches run on the Databricks arm (`--provider databricks`) unless the owner names another route | prose |
| R21 Live before tests | After code is written, no test is written or run until development is live-checked from several workspaces (main in `~/.claude`, another repo, `/tmp`; a subagent; each external agent); tests then wait for the owner's yes | prose |
