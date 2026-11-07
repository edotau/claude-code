export const meta = {
  name: 'code-engineer',
  description: 'The house dispatch: parallel Explore intel → implementer → test repair → new regression tests proven by mutation → gate loop + cross-package check',
  whenToUse:
    'USE: a MEDIUM+ code change end to end in one package set: Explore intel, claude implementer, a claude test-repair agent fixes the existing tests, a new-test agent writes mutation-proven regression/feature tests, gate loop (plus internal/schemas when a writer changed) and a cross-package build/vet/test of importers. CALL: Workflow({scriptPath: "~/.claude/workflows/code-engineer.js", args: {task, cwd, scope: ["internal/x"]}}). OPTIONAL: packages, gate (auto-detected), maxIterations, tests:false, steps {intel, impl, test, newtest, gate}: each {model, effort}; e.g. steps:{impl:{model:"opus"}}. RETURNS {status: blocked | error | green | red | stalled | capped, route, decisions, report, fixed, red, brief, impl, tests, newtests, gate, cross}. Details: saved-workflows.md.',
  phases: [
    { title: 'Intel', detail: 'two Explore readers with different lenses: seams/callers vs tests/guards' },
    { title: 'Build', detail: 'implementer (non-test files), test repair (existing tests), then new tests proven with claude-code test mutate' },
    { title: 'Gate', detail: 'green-keeper-loop over the touched packages, then schemas and importer-package checks' },
  ],
}

// ── Args ────────────────────────────────────────────────────────────────────
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
// The registry imports the script to read its meta, so a missing arg must not throw at module top level.
if (!a.task) return { error: 'code-engineer needs args.task' }
// ── stepOf: copied verbatim into code-analyzer, code-engineer, green-keeper-loop, code-evaluator ──
// stepOf(steps, name, defaults, {allowed}) -> {model, effort}; claude only, any bad input throws Error naming stage/field.
const stepOf = (steps, name, defaults, opt) => {
  const S_CLAUDE = ['haiku', 'sonnet', 'opus', 'fable']
  const S_EFFORT = ['low', 'medium', 'high', 'xhigh', 'max']
  const S_FIELDS = ['model', 'effort']
  const allowed = (opt && opt.allowed) || []
  const d = defaults || {}
  if (steps !== undefined && (!steps || typeof steps !== 'object' || Array.isArray(steps))) throw new Error('steps must be an object')
  for (const k of Object.keys(steps || {})) if (!allowed.includes(k)) throw new Error(`steps.${k}: unknown stage (allowed: ${allowed.join('|')})`)
  const where = `steps.${name}`
  const v = (steps || {})[name] === undefined ? {} : steps[name]
  if (!v || typeof v !== 'object' || Array.isArray(v)) throw new Error(`${where} must be an object`)
  for (const k of ['agent', 'profile']) if (k in v) throw new Error(`${where}.${k}: external agents were removed from workflows (claude only; use /code-worker:codex etc. outside the workflow)`)
  for (const k of Object.keys(v)) if (!S_FIELDS.includes(k)) throw new Error(`${where}.${k}: unknown field (allowed: ${S_FIELDS.join('|')})`)
  if (v.effort !== undefined && !S_EFFORT.includes(v.effort)) throw new Error(`${where}.effort must be ${S_EFFORT.join('|')}`)
  const model = v.model !== undefined ? v.model : d.model || 'sonnet'
  if (!S_CLAUDE.includes(model)) throw new Error(`${where}.model must be ${S_CLAUDE.join('|')}`)
  return { model, effort: v.effort !== undefined ? v.effort : d.effort || 'medium' }
}
// ── end stepOf ──
if (a.implModel !== undefined) return { status: 'error', error: 'code-engineer: implModel was replaced by steps.impl.model' }
let ST
try {
  const opt = { allowed: ['intel', 'impl', 'test', 'newtest', 'gate'] }
  ST = {
    intel: stepOf(a.steps, 'intel', { model: 'sonnet', effort: 'low' }, opt),
    impl: stepOf(a.steps, 'impl', { model: 'sonnet', effort: 'high' }, opt),
    test: stepOf(a.steps, 'test', { model: 'sonnet', effort: 'medium' }, opt),
    newtest: stepOf(a.steps, 'newtest', { model: 'sonnet', effort: 'medium' }, opt),
    gate: stepOf(a.steps, 'gate', { model: 'sonnet', effort: 'medium' }, opt),
  }
} catch (e) { return { status: 'error', error: 'code-engineer: ' + e.message } }
const TASK = a.task
const CWD = a.cwd || ''
const SCOPE = Array.isArray(a.scope) && a.scope.length ? a.scope : []
const PKGS = Array.isArray(a.packages) && a.packages.length ? a.packages : SCOPE
if (a.tests !== undefined && typeof a.tests !== 'boolean') return { error: 'code-engineer args.tests must be a boolean' }
const RUN_TESTS = a.tests !== false
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
// Passed to every child so none re-reads it; a failed read passes 'unknown' (no downshift anywhere).
const CLAUDE_STATE = routeState && typeof routeState.state === 'string' ? routeState.state : 'unknown'

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
    { label: `intel:${l.key}`, phase: 'Intel', schema: BRIEF_SCHEMA, agentType: 'Explore', model: routeModel(ST.intel.model), effort: ST.intel.effort } // Explore alone runs opus
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
const IMPL_PROMPT =
  `${context}\nYou are the IMPLEMENTER. Edit non-test source files only — never test files or fixtures (` + TEST_GLOBS + `); the test agent owns them. ` +
  `Surgical: touch only what the task requires, match existing style. Run the repo's formatter and linter on each file you touch (whichever it has: make fmt/lint, gofmt + go vet, ruff, eslint). Report NEEDS_CONTEXT before editing if anything is unclear; stop rather than guess at a new design decision. ` +
  `Before a behavior change write TASK: code does X; check expects Y; spec says Z (Y vs Z disagree = stop, report). ` +
  `After a defect fix write ISSUE: searched P, found N. Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 4-6). ` +
  `Status DONE | DONE_WITH_CONCERNS | NEEDS_CONTEXT | BLOCKED; put Ran on: <agent (model) · fallback none|who>, files changed, verification output and the TASK/ISSUE facts as plain sentences in the summary; list anything left undone.`
const impl = await agent(
  IMPL_PROMPT,
  { label: 'build:impl', phase: 'Build', schema: RESULT_SCHEMA, agentType: 'code-worker:claude', model: routeModel(ST.impl.model), effort: ST.impl.effort }
)
const implBlocked = !impl || impl.status === 'BLOCKED' || impl.status === 'NEEDS_CONTEXT'
const TEST_PROMPT = () =>
  `${context}\nImplementer result (already landed on disk): status ${impl.status}; files changed: ${(impl.filesChanged || []).join(', ') || 'none'}; summary: ${impl.summary}\n` +
  `You are the test-repair agent, REPAIR only: fix existing tests and fixtures (` + TEST_GLOBS + `) the implementation broke; write NO new tests (the main thread adds them after its live check). Never edit non-test files (the implementer owns them and has finished). ` +
  `Follow ~/.claude/agents/copilot.md → TEST (make the test-wrong vs code-wrong call; never edit non-test files). Status DONE | DONE_WITH_CONCERNS | NEEDS_CONTEXT | BLOCKED; include Ran on: in the summary. ` +
  `Run only the scoped tests for: ` + (PKGS.join(' ') || 'the touched packages/modules') + `; never the full suite.`
const tests = (implBlocked || !RUN_TESTS) ? null : await agent(
  TEST_PROMPT(),
  { label: 'build:tests', phase: 'Build', schema: RESULT_SCHEMA, agentType: 'code-worker:claude', model: routeModel(ST.test.model), effort: ST.test.effort }
)
const NEWTEST_PROMPT = () =>
  `${context}\nImplementer result (landed on disk): status ${impl.status}; files changed: ${(impl.filesChanged || []).join(', ') || 'none'}; summary: ${impl.summary}\n` +
  `You are the NEW-TEST agent. Existing tests are already repaired. For this bug fix or feature (see \`git diff\` of the changed files) write ONE regression/feature test per behavior change in *_test.go files that calls the real code and fails without the change; ` +
  `prove each with \`claude-code test mutate <file> <old> <new> --run <RE> ./<pkg> </dev/null\` (exit 0 = caught). Each test under complexity 10; edit only test files. ` +
  `Status DONE | DONE_WITH_CONCERNS | NEEDS_CONTEXT | BLOCKED; the summary lists each new test and its mutate exit code, plus Ran on:. Pure refactors needing no test: say so, status DONE.`
const newtests = (implBlocked || !RUN_TESTS) ? null : await agent(
  NEWTEST_PROMPT(),
  { label: 'build:newtests', phase: 'Build', schema: RESULT_SCHEMA, agentType: 'code-worker:claude', model: routeModel(ST.newtest.model), effort: ST.newtest.effort }
)
if (implBlocked) log('build: impl blocked or empty — test agents skipped')
else if (!RUN_TESTS) log('build: args.tests=false — test agents skipped')
log(`build: impl=${impl ? impl.status : 'none'} tests=${tests ? tests.status : 'none'} newtests=${newtests ? newtests.status : 'none'}`)

// Shared result shape (route/decisions/report/fixed/red) on top of the legacy status field.
const finish = (status, gate, cross) => {
  const decisions = [
    impl ? `impl ${impl.status}: ${(impl.filesChanged || []).length} file(s) changed` : 'impl returned nothing',
    !RUN_TESTS ? 'tests:false — no test repair, no new tests' : implBlocked ? 'tests skipped: impl blocked' :
      `test repair ${tests ? tests.status : 'none'}; new tests ${newtests ? newtests.status : 'none'} (mutation-proven only if its summary lists mutate exit 0)`,
    gate ? `gate ${gate.exit}` : 'gate not run',
    cross ? `cross-package check ${cross.green ? 'green' : 'RED'}: ${cross.output}` : 'cross-package check not run',
  ]
  const ok = status === 'green'
  const files = (impl && impl.filesChanged) || []
  const report = [`# code-engineer: ${status}`, `Task: ${TASK}`, '', ...decisions.map(d => '- ' + d), '', `Files: ${files.join(', ') || 'none'}`].join('\n')
  return { route: 'code-engineer', status, decisions, report, fixed: ok ? files : [], red: ok ? [] : (files.length ? files : [TASK]), brief, impl, tests, newtests, gate, cross }
}

// ── Phase 3: Gate (ADK LoopAgent via the saved green-keeper-loop workflow) ─────────────────────────
phase('Gate')
if (implBlocked) return finish('blocked', null, null)
const gate = await workflow('green-keeper-loop', { claudeState: CLAUDE_STATE, gate: GATE, cwd: CWD || undefined, scope: SCOPE.length ? SCOPE : [...new Set(brief.files.map(f => f.includes('/') ? f.slice(0, f.lastIndexOf('/')) : '.'))], maxIterations: MAX_ITER, steps: a.steps && a.steps.gate ? { fix: { model: ST.gate.model, effort: ST.gate.effort } } : undefined })
// Cross-package check: a scoped gate misses schemas and importer breakage; bounded to the direct importers of the changed packages.
const cross = await agent(
  `${SHARED_TREE}\nEdit nothing. Run \`set -o pipefail; go build ./... && go vet ./...\`. Then for the packages of the files changed (${(impl.filesChanged || []).join(', ')}) ` +
  `list direct importers with \`go list -f '{{.ImportPath}} {{join .Imports " "}}' ./...\` (match each changed package's import path), at most 15, and run \`go test -count=1\` on the changed packages plus those importers. ` +
  `If \`git diff\` changes a function that writes a file, also run \`go test -count=1 ./internal/schemas/\`. green=true only if every command exited 0; output = failing commands' last lines or "ok".`,
  { label: 'gate:cross', phase: 'Gate', schema: { type: 'object', additionalProperties: false, required: ['green', 'output'], properties: { green: { type: 'boolean' }, output: { type: 'string' } } }, model: routeModel(ST.gate.model), effort: ST.gate.effort }
).catch(() => null)
// A green gate over a not-finished implementer proves nothing.
const implDone = impl.status === 'DONE' || impl.status === 'DONE_WITH_CONCERNS'
const status = !gate ? 'error' : gate.exit !== 'green' ? gate.exit : (!implDone || !cross || !cross.green) ? 'red' : 'green'
return finish(status, gate, cross)
