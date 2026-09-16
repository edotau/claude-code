export const meta = {
  name: 'implement-and-verify',
  description: 'The house dispatch: parallel Explore intel → implementer ‖ test-repair (same message) → gate loop',
  whenToUse:
    'Any MEDIUM+ code change (CLAUDE.md guideline 6 + rules/standards/quality.md). ADK shape: Sequential[Parallel intel, Parallel impl‖tests, Loop gate]. ' +
    'args: {task: "...", cwd, scope: ["internal/x", …], packages: ["./internal/x/"], gate?, maxIterations?}. The main session still owns the live check.',
  phases: [
    { title: 'Intel', detail: 'two Explore readers with different lenses: seams/callers vs tests/guards' },
    { title: 'Build', detail: 'implementer (non-test files) ‖ test-repair (test files) in one parallel step' },
    { title: 'Gate', detail: 'gate-loop workflow over the touched packages' },
  ],
}

// ── Args ────────────────────────────────────────────────────────────────────
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
if (!a.task) throw new Error('implement-and-verify needs args.task')
const TASK = a.task
const CWD = a.cwd || ''
const SCOPE = Array.isArray(a.scope) && a.scope.length ? a.scope : []
const PKGS = Array.isArray(a.packages) && a.packages.length ? a.packages : SCOPE.map(s => './' + s.replace(/^\.\//, '') + '/')
const GATE = a.gate || `go build ./... && go vet ${PKGS.join(' ') || './...'} && go test ${PKGS.join(' ') || './...'}`
const MAX_ITER = a.maxIterations || 3
const SHARED_TREE =
  'Shared live tree: never git stash|checkout|reset|restore|clean|commit, never make install|test, never edit settings.json. ' +
  (CWD ? 'Prefix every shell command with `cd ' + CWD + ' &&`. ' : '') + 'Comments 1 line ≤120 chars.'

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
    status: { type: 'string', enum: ['done', 'partial', 'blocked'] },
    filesChanged: { type: 'array', items: { type: 'string' } },
    summary: { type: 'string' },
    leftUndone: { type: 'array', items: { type: 'string' } },
  },
}

// ── Phase 1: Intel (ADK ParallelAgent; read-only, two lenses, barrier because Build needs both) ─
phase('Intel')
const LENSES = [
  { key: 'seams', prompt: 'the call graph: every caller, registry row, completion table and doc that names the symbols involved' },
  { key: 'guards', prompt: 'the tests and guard tests (grep *_test.go for pinned filenames, goldens, walk-the-tree guards) that constrain it' },
]
const briefs = (await parallel(LENSES.map(l => () =>
  agent(
    `Read-only scouting for a code change${CWD ? ' in ' + CWD : ''}. Task: ${TASK}\nScope hint: ${SCOPE.join(', ') || 'none given'}\n` +
    `Your lens: ${l.prompt}. Return files, seams (file:line) and risks. Do not edit anything.`,
    { label: `intel:${l.key}`, phase: 'Intel', schema: BRIEF_SCHEMA, agentType: 'Explore', effort: 'low' }
  )
))).filter(Boolean)
const brief = {
  files: [...new Set(briefs.flatMap(b => b.files))],
  seams: [...new Set(briefs.flatMap(b => b.seams))],
  risks: [...new Set(briefs.flatMap(b => b.risks))],
}
log(`intel: ${brief.files.length} files, ${brief.seams.length} seams, ${brief.risks.length} risks`)

// ── Phase 2: Build (ADK ParallelAgent; disjoint ownership — impl owns code, tests owns *_test.go) ─
phase('Build')
const context = `Task: ${TASK}\nFiles: ${brief.files.join(', ')}\nSeams:\n- ${brief.seams.join('\n- ')}\nRisks:\n- ${brief.risks.join('\n- ')}\n${SHARED_TREE}`
const [impl, tests] = await parallel([
  () => agent(
    `${context}\nYou are the IMPLEMENTER. Edit non-test Go/other source files only — never *_test.go or testdata/ (a concurrent test agent owns them). ` +
    `Surgical: touch only what the task requires, match existing style. Run gofmt -l and go vet on each package you touch. Report files changed and anything left undone.`,
    { label: 'build:impl', phase: 'Build', schema: RESULT_SCHEMA, model: 'opus', effort: 'high' }
  ),
  () => agent(
    `${context}\nYou are the TEST-REPAIR agent. Edit *_test.go and testdata/ only — never non-test files (a concurrent implementer owns them; poll the tree and re-read before asserting). ` +
    `Repair existing tests the change breaks or makes stale, regenerate goldens by their documented procedure, add coverage sized to the change. ` +
    `Run only the scoped tests: ${PKGS.join(' ') || 'the touched packages'}. Report tests added/repaired and anything the implementation left inconsistent.`,
    { label: 'build:tests', phase: 'Build', schema: RESULT_SCHEMA, model: 'sonnet', effort: 'medium' }
  ),
])
log(`build: impl=${impl ? impl.status : 'none'} tests=${tests ? tests.status : 'none'}`)

// ── Phase 3: Gate (ADK LoopAgent via the saved gate-loop workflow) ─────────────────────────
phase('Gate')
const gate = await workflow('gate-loop', { gate: GATE, cwd: CWD || undefined, scope: SCOPE.length ? SCOPE : brief.files, maxIterations: MAX_ITER })

return { brief, impl, tests, gate }
