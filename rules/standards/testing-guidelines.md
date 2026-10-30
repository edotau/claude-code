# Testing Guidelines

Order, always: **run it live → pin what you observed → prove the test bites.** Gate order: `quality.md` →
Verification Order. Optional loops and tooling: `skills/subagent-workflows/references/test-and-fix.md`.

## Live first, then tests — no core-contract exception

**Run the change on its real surface (verb, hook, endpoint, make target) with real input and SHOW the output
before writing any NEW test.** A test written before the live run pins your guess, not the behavior.

- **A bug fix's live run is the RED:** reproduce it, state the root cause ("X because Y"), fix, re-run the same
  command; the before/after output is the evidence.
- **Repairing existing tests** the change breaks is not new testing — it runs concurrently (`copilot`).
- **No real input:** say so, ask for exactly what's needed, and mark anything written as unverified.
- **No runnable surface:** pin the spec as Given/When/Then in a test that imports the code, and prove its teeth.

## Scope — logic, not a coverage number

Per feature, one end-to-end test plus the corner cases found while building it; per bug, one regression test that
fails without the fix. Nothing more unless asked. Renames, moves and deletions need none; a refactor that breaks
only tests means rewriting them against behavior, not keeping fixtures for states the code can no longer produce.
A test the `review-simplicity` hook flags (complexity > 10, > 50 lines) is testing too much at once: cut it to the
corner cases you found or split it by behavior — never raise the limit.

## A test must guard

- **Call the code under test** (`got := renderVaultText(doc, …)`), never an inline copy of its logic — that
  passes with the fix reverted.
- **Prove the teeth:** `claude-code test mutate <file> <old> <new> [--run RE] <pkg>` (exit 0 = the suite caught the
  defect); check the failure message names it. Never edit-and-restore a tracked file. A test that can't fail is
  deleted.
- **Build the defect's worst case**; derive expected values from the fixture and drive parsers with real bytes.
  Equivalence tests assert the fast path actually ran (a call count, a cache key) against a deterministic oracle.
- **Mock only real boundaries** (network, model calls, clock, external CLIs, process scans), at the consumer; run
  everything else against a hermetic config dir (`t.TempDir()`, `CLAUDE_CONFIG_DIR`). More than two mocks is a
  missing seam. In Go the seam is a package var, swapped and restored:

  ```go
  var peerAgents = worktree.PeerAgents // production: scans live processes

  orig := peerAgents
  peerAgents = func(string, []string) ([]worktree.Proc, error) { return []worktree.Proc{{PID: 4242, Comm: "claude"}}, nil }
  t.Cleanup(func() { peerAgents = orig })
  ```

- **Never spawn the test binary** — `os.Executable()` is `go test`'s; stub every daemon-spawn seam.
- **At a boundary, error paths first**, asserting shape (required fields present, sensitive ones absent); a crash
  or 500 on bad input is a finding.
- **Stop and ask** rather than encode a guess when behavior isn't determinable, or the change touches an API/DB
  contract, auth, permissions or PII.

## When a test fails

The test is guilty until its expectation traces to the spec, the request or observed behavior — say which. Rerun a
red seen under parallel load solo before calling it a regression. Never weaken an assertion, skip a test or edit a
migration to go green; after 3 fix attempts on one test, report it.

## Test agent — only when the change needs one

A feature or bug fix gets a test agent (`copilot`, or `code-worker` for a scoped suite) dispatched in the SAME
message as the implementer — dispatched afterwards, the concurrency is lost. Renames, moves, deletions and
refactors get none; the implementer keeps the build green. The main thread owns the live check; the test agent
repairs broken tests at once and writes new ones after the live check. It ends its turn after taking the
baseline; the manager's "landed" `SendMessage` (files + signatures) resumes it — no polling. Roughly one per
suite under change; they count toward the dispatch cap. Not for exploration, docs or review-only work; a target
with no suite is reported, not given a new framework, and planned-but-unwritten tests are listed, never silent.
