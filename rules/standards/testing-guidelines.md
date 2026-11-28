---
paths:
  - "**/*_test.go"
  - "**/test_*.py"
  - "**/*_test.py"
  - "**/*.test.ts"
  - "**/*.spec.ts"
  - "**/tests/**"
  - "**/testdata/**"
---

# Testing Guidelines

Order, always: **run it live → pin what you observed** (gate order: `quality.md` → Verification Order). Fewer tests
that guard real breakage beat many that need repair: "we fall into the trap of always spending more time on test
repairs for tests that we didnt need in the first place" (owner).

## Live first, then tests

**Run the change on its real surface (verb, hook, endpoint, make target) with real input and show the output before
writing any NEW test.** A test written first pins your guess, not the behavior.

- **A bug fix's live run is the RED:** reproduce it, state the root cause ("X because Y"), fix, re-run the same
  command; the before/after output is the evidence.
- **Repairing existing tests** the change breaks is not new testing; the implementer or a repair leaf does it.
- **No real input:** say so, ask for exactly what is needed, and mark anything written as unverified.

## Several worktrees: collapse, then test once

Here n counts lanes in a parallel wave.
Tree selection, commits, collection, landing and cleanup: `rules/harness/workflow-tree.md` → Worktrees.

Before hand-back, a subagent validates only its owned work, cheaply (never repo-wide `./...` or package test suites):

- `rtk go build <its packages>` and `go vet <its packages>`.
- `claude-code agents diff --check --gates lint,complexity --path <its files>`.
- No test of any shape, and no test writing: the main session tests.

Subagents, including test-repair leaves, never run a package suite, `go build ./...`, or the full suite (`agents diff
--check` without `--gates` runs both `go build ./...` and package tests, so leaves pass `--gates lint,complexity`).
`safety-scan` denies direct whole-suite runs to every agent and in every linked worktree. Only the final landing step
below runs the test gate once after the review loop finishes.

Heavy runs (capacity benchmark, long soak, timing gate) run on their own machine: a podman pod, Databricks cluster
or another host over ssh. Never run them where sessions work, in either case below.

### n = 1

The collector is the only worktree, with no sibling lanes. Once its work is complete, run `Skill(simplify)` and the
other-family Goldfish review loop (`quality.md` → Completion) on the collector **before** running the test suite, so
review fixes do not re-trigger test runs. Only at the final step does the main session run `claude-code worktree land
<collector>` (which runs the scoped package + direct-importer test gate once in the collector and queues the single
background post-land audit). A single finished collector does not wait for another wave.

### n > 1

Each lane subagent runs only the package-scoped static checks and exact-name test above before handing back. Collapse
all finished lanes of the wave in one batch call (`claude-code worktree collapse <collector> <lane1> <lane2> …`), which
runs `go build ./...` and `go vet` once across the merged batch. Do not run reviews or package suites between arrivals.

Only after every lane of the wave is in the collector does the main session run `Skill(simplify)` and the other-family
Goldfish review loop (R28) on the merged diff. Once the review loop is clean, the main session runs the final landing
step (`claude-code worktree land <collector>`) once for the entire wave, never per lane or per review round.

## What gets a test, and what never does

- **Gets one:** behaviour whose break would do damage: a parser, a merge that could lose data, a security boundary,
  a routing decision. One basic test per feature, seconds at most, calling the real code (never a copy of its logic)
  and failing when that behaviour is removed; a test that still passes then is deleted. Corner cases and per-bug
  regressions are proposed in the hand-back (the case, what it guards, its runtime), not written.
- **Never gets one** (owner, 2026-11-26); check it live instead:
  - **Configuration and preference:** where a setting lives, a value the owner picked, a layout.
  - **Copies of an asset:** golden files, hash pins, copied template, help or notice text. Editing a config, template
    or text file never forces a test edit.
  - **Plumbing:** settings merges, config fills, notices.
  - **Renames, moves and deletions.** A refactor that breaks only tests rewrites them against behaviour.
- **A leaf or workflow test stage** writes only repairs of tests its edits broke, plus the feature's basic test.

## A test must guard

- **Shapes and edges, not values** — Build the defect's worst case: empty, one element, boundary, malformed,
  concurrent, present-but-empty. Derive expected values from fixtures; feed parsers real bytes.
- **Equivalence** — Assert the fast path actually ran (call count or cache key) against a deterministic oracle.
- **Hermetic boundaries** — Never reach the internet or machine services (proxy, Magnitude's local model, Jev,
  Tailscale, `ssh`), or spawn a real agent CLI. Mock only real boundaries (network, model calls, clock, external
  CLIs, process scans) at the consumer. Use an in-process `httptest` server or test-owned stub transport, dialer or
  binary, with replies defined in the test. If the code has no seam and the test is not worth one, delete the test.
- **Network enforcement** — Keep `internal/platform/nonet` refusing DNS in every Go test binary; audit loopback by
  running the suite once under a sandbox (macOS `sandbox-exec` blocking remote network and those service ports).
- **Owned data only** — Read and write only test-created data: `testdata/` fixtures, files in `t.TempDir()` or
  structs in the test. Never access the live memory store, vault, `state/`, settings, plans or a user cache, even
  read-only. Put real-data checks in a verb's preflight or a `doctor` row.
- **Minimal seams** — Run nonmocked code against a hermetic config dir (`t.TempDir()`, `CLAUDE_CONFIG_DIR`).
  Treat more than two mocks as a missing seam; in Go, use a package var swapped and restored with `t.Cleanup`.
- **Fast** — Keep each test to milliseconds and added package time to at most 0.5 s. Never sleep, run a daemon or
  spawn a binary, including the test binary; shrink timeouts through package vars.
- **Every system** — Use no single-OS test or seam; state each system's expectation in one body. Keep runtime skips
  per `go.md` → Tests.
- **No credential shapes** — Use plain gibberish; never use values starting with vendor prefixes (`dapi`, `dose`,
  `ghp_`, `github_pat_`, `sk-`, `tskey-`, `AKIA`, `xox`, `eyJ`, …) or matching a vendor credential's length and
  alphabet, even assembled at runtime. Check token-shape code live.
- **Documentation hosts** — Use made-up hostnames (`devhost`) on documentation addresses (`192.0.2.0/24`,
  `198.51.100.0/24`, `203.0.113.0/24`, `2001:db8::/32`).
- **Boundary errors first** — Test error paths first at boundaries; assert required fields are present and sensitive
  fields absent.
- **Stop and ask** — Ask rather than guess when behaviour is indeterminable or changes touch API or DB contracts,
  auth, permissions or PII.

## TEST

### When a test fails

When a test fails, it is guilty until its expectation traces to the spec, the request or observed behaviour; say
which. A repair leaf owns only the named test files; the brief gives the failures and the intended behaviour.

1. Use the captured failures; a repair leaf reruns only the test it repairs, by exact name. Re-run a red seen under
   parallel load alone before calling it a regression. Suite scope: Several worktrees: collapse, then test once.
2. Classify each failure with evidence: **test wrong** (the contract changed on purpose: update the expectation, keep
   the intent), **code wrong** (report the regression, leave the test) or **vacuous** (it no longer exercises its
   subject: report the lost guard).
3. Repair only test-wrong failures, re-run, and report before and after counts from real output; after 3 attempts on
   one test, report it instead.

Never change product code to green a repair, weaken an assertion, skip or delete a test to clear a failure, edit a
migration to go green, or invent a framework.
