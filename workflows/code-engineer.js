export const meta = {
  name: 'code-engineer',
  description: 'The house dispatch: parallel Explore intel → implementer → copilot test repair (after impl lands) → scoped gate loop',
  whenToUse:
    'Any MEDIUM+ code change (CLAUDE.md guideline 6 + rules/standards/quality.md). ADK shape: Sequential[Parallel intel, impl → test repair, Loop gate]. args: {task, cwd, scope: [dirs], packages?, gate? (auto-detected from Makefile/go.mod/pyproject/package.json/Cargo.toml), maxIterations?, implModel?: "opus" only after a sonnet attempt failed}. Works in any repo via cwd. The test agent only repairs existing tests the change broke; the main session owns the live check, then writes new tests. Does not chain into code-analyzer (workflow() nests one level); run code-analyzer as the next top-level call before merge.',
  phases: [
    { title: 'Intel', detail: 'two Explore readers with different lenses: seams/callers vs tests/guards' },
    { title: 'Build', detail: 'implementer (non-test files), then copilot test repair (existing test files only) with the impl result' },
    { title: 'Gate', detail: 'green-keeper-loop workflow over the touched packages' },
  ],
}

// ── Args ────────────────────────────────────────────────────────────────────
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
// The registry imports the script to read its meta, so a missing arg must not throw at module top level.
if (!a.task) return { error: 'code-engineer needs args.task' }
const TASK = a.task
const CWD = a.cwd || ''
const SCOPE = Array.isArray(a.scope) && a.scope.length ? a.scope : []
const PKGS = Array.isArray(a.packages) && a.packages.length ? a.packages : SCOPE
const GATE = a.gate // undefined → green-keeper-loop detects it from the repo
const MAX_ITER = Number.isInteger(a.maxIterations) && a.maxIterations > 0 ? a.maxIterations : 3
const TEST_GLOBS = '*_test.go, testdata/, test_*.py, *_test.py, tests/, conftest.py, *.test.*, *.spec.*, __tests__/, fixtures/'
const SHARED_TREE =
  'Shared live tree: never git stash|checkout|reset|restore|clean|commit, never run install/deploy/release targets or edit tool config. ' +
  (CWD ? 'Prefix every shell command with `cd ' + CWD + ' &&`. ' : '') + 'Comments 1 line ≤120 chars.'

// ── Pressure: Claude account state read once; low/exhausted steps every Claude stage down one model ──
const routeState = await agent(
  `Run exactly: ${CWD ? 'cd ' + CWD + ' && ' : ''}claude-code gateway router --format json --agent claude --fallback none --model sonnet. ` +
    `It prints one JSON object; state = candidates[0].state when candidates is non-empty, else the skipped[] entry whose agent ` +
    `is "claude" and its gate value, else "unknown". Never run anything else.`,
  { label: 'route-state', phase: 'Intel', schema: { type: 'object', additionalProperties: false, required: ['state'], properties: { state: { type: 'string' } } }, model: 'haiku', effort: 'low' }
).catch(() => null)
const MODEL_DOWN = { opus: 'sonnet', sonnet: 'haiku', haiku: 'haiku' }
const PRESSURE = !!(routeState && (routeState.state === 'low' || routeState.state === 'exhausted'))
const routeModel = m => (PRESSURE && MODEL_DOWN[m]) ? MODEL_DOWN[m] : m
if (PRESSURE) log(`pressure: claude account ${routeState.state} — stepping Claude stages down one model`)
else if (!routeState) log('pressure: route read failed — no downshift (fail open)')

const BRIEF_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['files', 'seams', 'risks'],
  properties: {
    files: { type: 'array', items: { type: 'string' }, description: 'repo-relative files the task must touch' },
    seams: { type: 'array', items: { type: 'string' }, description: 'callers/tests/guards that constrain the change, file:line each' },
    risks: { type: 'array', items: { type: 'string' }, description: 'what a naive change would break' },
  },
}
const RESULT_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['status', 'filesChanged', 'summary', 'leftUndone'],
  properties: {
    status: { type: 'string', enum: ['DONE', 'DONE_WITH_CONCERNS', 'NEEDS_CONTEXT', 'BLOCKED'] },
    filesChanged: { type: 'array', items: { type: 'string' } },
    summary: { type: 'string', minLength: 60, description: 'what changed, what you ran and its last lines, Ran on:, the TASK/ISSUE facts as plain sentences — a one-word summary is a defect' },
    leftUndone: { type: 'array', items: { type: 'string' } },
  },
}

// ── Phase 1: Intel (ADK ParallelAgent; read-only, two lenses, barrier because Build needs both) ─
phase('Intel')
const LENSES = [
  { key: 'seams', prompt: 'the call graph: every caller, registration/config table and doc that names the symbols involved' },
  { key: 'guards', prompt: 'the tests and guard tests (grep the test files (' + TEST_GLOBS + ') for pinned filenames, goldens, fixtures) that constrain it' },
]
const briefs = (await parallel(LENSES.map(l => () =>
  agent(
    `Read-only scouting for a code change${CWD ? ' in ' + CWD : ''}. Task: ${TASK}\nScope hint: ${SCOPE.join(', ') || 'none given'}\n` +
    `Your lens: ${l.prompt}. Return files, seams (file:line) and risks. Do not edit anything.`,
    { label: `intel:${l.key}`, phase: 'Intel', schema: BRIEF_SCHEMA, agentType: 'Explore', model: routeModel('sonnet'), effort: 'low' } // Explore alone runs opus
  )
))).filter(Boolean)
if (!briefs.length) log('intel: both Explore readers returned nothing — building blind from the task text')
const brief = {
  files: [...new Set([...briefs.flatMap(b => b.files), ...SCOPE])],
  seams: [...new Set(briefs.flatMap(b => b.seams))],
  risks: [...new Set(briefs.flatMap(b => b.risks))],
}
log(`intel: ${brief.files.length} files, ${brief.seams.length} seams, ${brief.risks.length} risks`)

// ── Phase 2: Build (ADK SequentialAgent; disjoint ownership — impl owns code, tests owns *_test.go, tests run after impl) ─
phase('Build')
const context = `Task: ${TASK}\nFiles: ${brief.files.join(', ')}\nSeams:\n- ${brief.seams.join('\n- ')}\nRisks:\n- ${brief.risks.join('\n- ')}\n${SHARED_TREE}`
const impl = await agent(
  `${context}\nYou are the IMPLEMENTER. Edit non-test source files only — never test files or fixtures (` + TEST_GLOBS + `); the test agent owns them. ` +
  `Surgical: touch only what the task requires, match existing style. Run the repo's formatter and linter on each file you touch (whichever it has: make fmt/lint, gofmt + go vet, ruff, eslint). Report NEEDS_CONTEXT before editing if anything is unclear; stop rather than guess at a new design decision. ` +
  `Before a behavior change write TASK: code does X; check expects Y; spec says Z (Y vs Z disagree = stop, report). ` +
  `After a defect fix write ISSUE: searched P, found N. Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 4-6). ` +
  `Status DONE | DONE_WITH_CONCERNS | NEEDS_CONTEXT | BLOCKED; put Ran on: <agent (model) · fallback none|who>, files changed, verification output and the TASK/ISSUE facts as plain sentences in the summary; list anything left undone.`,
  { label: 'build:impl', phase: 'Build', schema: RESULT_SCHEMA, agentType: 'code-worker', model: routeModel(a.implModel || 'sonnet'), effort: 'high' }
)
const implBlocked = !impl || impl.status === 'BLOCKED' || impl.status === 'NEEDS_CONTEXT'
const tests = implBlocked ? null : await agent(
  `${context}\nImplementer result (already landed on disk): status ${impl.status}; files changed: ${(impl.filesChanged || []).join(', ') || 'none'}; summary: ${impl.summary}\n` +
  `You are the TEST agent (copilot), REPAIR only: fix existing tests and fixtures (` + TEST_GLOBS + `) the implementation broke; write NO new tests (the main thread adds them after its live check). Never edit non-test files (the implementer owns them and has finished). ` +
  `Follow ~/.claude/agents/copilot.md → TEST (make the test-wrong vs code-wrong call; never edit non-test files). Status DONE | DONE_WITH_CONCERNS | NEEDS_CONTEXT | BLOCKED; include Ran on: in the summary. ` +
  `Run only the scoped tests for: ` + (PKGS.join(' ') || 'the touched packages/modules') + `; never the full suite.`,
  { label: 'build:tests', phase: 'Build', schema: RESULT_SCHEMA, agentType: 'code-worker', model: routeModel('sonnet'), effort: 'medium' }
)
if (implBlocked) log('build: impl blocked or empty — test agent skipped')
log(`build: impl=${impl ? impl.status : 'none'} tests=${tests ? tests.status : 'none'}`)

// ── Phase 3: Gate (ADK LoopAgent via the saved green-keeper-loop workflow) ─────────────────────────
phase('Gate')
if (implBlocked) return { brief, impl, tests, gate: null, status: 'blocked' }
const gate = await workflow('green-keeper-loop', { gate: GATE, cwd: CWD || undefined, scope: SCOPE.length ? SCOPE : brief.files, maxIterations: MAX_ITER })

return { brief, impl, tests, gate, status: gate ? gate.exit : 'error' }
