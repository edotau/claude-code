export const meta = {
  name: 'test-and-fix-fanout',
  description: 'Dynamically discover Python test targets across workspaces, then fan out one test->fix->verify agent per target concurrently; report a consolidated pass/fail matrix',
  whenToUse: 'Run the /test-and-fix loop across many repos at once. Pass args to scope which repos; omit to auto-discover all repos with a .venv + test target.',
  phases: [
    { title: 'Discover', detail: 'scan candidate repos for .venv + pytest/make test target; emit work-list' },
    { title: 'TestFix', detail: 'per target: run tests, fix failures one at a time (max 3/test), re-verify' },
    { title: 'Report', detail: 'consolidated pass/fail matrix + residual failures' },
  ],
}

// ── Candidate workspaces ────────────────────────────────────────────────────
// args carries the real list: an array of {name, dir} or dir strings (Workflow scripts can't read files).
// The fallback is a placeholder.
const DEFAULT_CANDIDATES = [
  { name: 'example-repo', dir: '~/my-workspace' },
]

// Normalize args -> candidate list. Accepts: undefined | [dir,...] | [{name,dir},...]
function resolveCandidates(a) {
  if (!a) return DEFAULT_CANDIDATES
  if (!Array.isArray(a)) return DEFAULT_CANDIDATES
  return a.map((x, i) =>
    typeof x === 'string'
      ? { name: x.split('/').filter(Boolean).pop() || `target-${i}`, dir: x }
      : { name: x.name || `target-${i}`, dir: x.dir }
  ).filter(c => c.dir)
}

const DISCOVERY_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['name', 'dir', 'runnable', 'testCommand', 'reason'],
  properties: {
    name: { type: 'string' },
    dir: { type: 'string' },
    runnable: { type: 'boolean', description: 'true if repo has a .venv AND a discoverable test target' },
    testCommand: { type: 'string', description: 'exact command to run the unit suite, e.g. "make test-unit" or ".venv/bin/python -m pytest -x -q"' },
    reason: { type: 'string', description: 'why runnable is true/false (which Makefile target / pytest config was found)' },
  },
}

const FIX_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['name', 'status', 'testsRun', 'failuresBefore', 'failuresAfter', 'filesChanged', 'summary'],
  properties: {
    name: { type: 'string' },
    status: { type: 'string', enum: ['pass', 'fixed', 'partial', 'fail', 'error', 'skipped'] },
    testsRun: { type: 'integer' },
    failuresBefore: { type: 'integer' },
    failuresAfter: { type: 'integer' },
    filesChanged: { type: 'array', items: { type: 'string' } },
    summary: { type: 'string', description: 'one-paragraph outcome: what failed, what was fixed, what remains' },
  },
}

const candidates = resolveCandidates(args)
log(`Discovering test targets across ${candidates.length} candidate workspace(s)`)

// ── Phase 1: Discover (parallel, read-only) ─────────────────────────────────
phase('Discover')
const discovered = await parallel(candidates.map(c => () =>
  agent(
    `You are scoping a Python repo for its unit-test command. DO NOT run tests or edit anything — read-only.\n` +
    `Repo: ${c.dir} (name: ${c.name})\n\n` +
    `Steps:\n` +
    `1. Check ${c.dir}/.venv/bin/python exists.\n` +
    `2. Read ${c.dir}/Makefile (if present) and find the fastest unit-test target. Prefer in order: test-unit, test-smoke, test-fast, test. Capture the literal recipe command name.\n` +
    `3. If no Makefile test target, check for pytest config (pyproject.toml/setup.cfg/pytest.ini) and a tests/ dir; the command would be ".venv/bin/python -m pytest -x -q".\n` +
    `4. Decide runnable = (.venv exists) AND (a test target or pytest+tests/ exists).\n` +
    `Return the structured verdict. testCommand must be the exact shell command to run from the repo root.`,
    { label: `discover:${c.name}`, phase: 'Discover', schema: DISCOVERY_SCHEMA, model: 'sonnet', effort: 'low' }
  ).then(v => v ? { ...v, dir: c.dir, name: c.name } : null)
))

const runnable = discovered.filter(Boolean).filter(d => d.runnable)
const skipped = discovered.filter(Boolean).filter(d => !d.runnable)
log(`Runnable: ${runnable.map(r => r.name).join(', ') || 'none'}`)
if (skipped.length) log(`Skipped (not runnable): ${skipped.map(s => `${s.name} (${s.reason})`).join('; ')}`)

if (!runnable.length) {
  return { discovered, runnable: [], results: [], note: 'No runnable test targets found.' }
}

// ── Phase 2: Test -> Fix -> Verify (one agent per target, concurrent) ────────
phase('TestFix')
const results = await parallel(runnable.map(t => () =>
  agent(
    `Run the /test-and-fix loop for ONE repo and report the outcome. Work ONLY inside ${t.dir}.\n\n` +
    `Repo: ${t.name}  (${t.dir})\n` +
    `Test command: ${t.testCommand}\n\n` +
    `Protocol (from the project's /test-and-fix command):\n` +
    `1. cd into the repo and run the test command. NEVER use bare python/pytest — always the repo's .venv/bin/ prefix (the make targets already do).\n` +
    `2. If all pass: status="pass", report counts, done.\n` +
    `3. If failures: fix ONE test at a time. Read the failing test + the source it covers. Decide root cause (test wrong vs impl wrong). Make the MINIMAL change. Re-run only that test with the .venv pytest. Max 3 attempts per test, then move on.\n` +
    `4. Do NOT touch migrations/ directories. Do NOT modify unrelated code. If a fix needs a DB migration or external service, mark it and skip.\n` +
    `5. After fixes, re-run the full test command to confirm.\n` +
    `6. If the repo has a "make fix" or "make format" target, run it to format your changes.\n\n` +
    `Set status: "pass" (green first try), "fixed" (all green after fixes), "partial" (some fixed, some remain), "fail" (none fixed), "error" (couldn't run). List every file you changed.`,
    { label: `testfix:${t.name}`, phase: 'TestFix', schema: FIX_SCHEMA, isolation: 'worktree', model: 'sonnet', effort: 'medium' }
  ).then(v => v || { name: t.name, status: 'error', testsRun: 0, failuresBefore: 0, failuresAfter: 0, filesChanged: [], summary: 'agent returned no result' })
))

// ── Phase 3: Report ─────────────────────────────────────────────────────────
phase('Report')
const matrix = results.filter(Boolean).map(r =>
  `${r.status === 'pass' || r.status === 'fixed' ? '✅' : r.status === 'partial' ? '🟡' : '🔴'} ` +
  `${r.name.padEnd(20)} ${r.status.padEnd(8)} run=${r.testsRun} before=${r.failuresBefore} after=${r.failuresAfter} changed=${r.filesChanged.length}`
).join('\n')
log('Consolidated matrix:\n' + matrix)

return {
  discovered: discovered.filter(Boolean).map(d => ({ name: d.name, runnable: d.runnable, reason: d.reason })),
  skipped: skipped.map(s => s.name),
  results: results.filter(Boolean),
  matrix,
}
