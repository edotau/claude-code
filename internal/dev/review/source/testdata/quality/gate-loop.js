export const meta = {
  name: 'gate-loop',
  description: 'LoopAgent: run a gate command, fix what fails inside a scope, re-run — until green or maxIterations',
  whenToUse:
    'The verify tail of any dispatch (ADK LoopAgent shape). args: {gate: "go build ./... && go vet ./... && go test ./internal/x/", cwd, scope: ["internal/x"], maxIterations: 3}. Also called inline by implement-and-verify and audit-fix-verify via workflow("gate-loop", …).',
  phases: [{ title: 'Gate', detail: 'one fixer per iteration: run the gate, fix root causes within scope, re-run' }],
}

// ── Args ────────────────────────────────────────────────────────────────────
// Every field has a default so the workflow runs bare; scope is the ONLY place the fixer may edit.
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const GATE = a.gate || 'go build ./... && go vet ./...'
const CD = a.cwd ? `cwd: ${a.cwd}. Prefix every command with \`cd ${a.cwd} &&\` (the shell cwd resets).\n` : ''
const SCOPE = Array.isArray(a.scope) && a.scope.length ? a.scope : ['(whole repo — scope not given; prefer the smallest fix)']
const MAX_ITER = Number.isInteger(a.maxIterations) && a.maxIterations > 0 ? a.maxIterations : 3
const RULES = a.rules ||
  'Never run git stash|checkout|reset|restore|clean|commit, never make install|test, never edit settings.json. ' +
  'Fix the implementation, not the test, unless the test is provably wrong. One root cause per edit; re-run only the gate.'

const GATE_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['status', 'failuresBefore', 'failuresAfter', 'filesChanged', 'residual', 'summary'],
  properties: {
    status: { type: 'string', enum: ['green', 'red', 'error'], description: 'green = gate exits 0 after this iteration' },
    failuresBefore: { type: 'integer', description: 'distinct failures the first gate run of this iteration reported' },
    failuresAfter: { type: 'integer', description: 'distinct failures the last gate run of this iteration reported' },
    filesChanged: { type: 'array', items: { type: 'string' } },
    residual: { type: 'array', items: { type: 'string' }, description: 'one line per failure still red, verbatim from the gate' },
    summary: { type: 'string', description: 'what failed, what was fixed, why anything remains' },
  },
}

// ── Loop (ADK LoopAgent: sub-agent per iteration, exit on the escalation signal or the cap) ────
phase('Gate')
const history = []
let last = null
for (let i = 1; i <= MAX_ITER; i++) {
  const prior = last && last.residual.length ? `\nStill red after iteration ${i - 1}:\n- ${last.residual.join('\n- ')}\n` : ''
  last = await agent(
    `Iteration ${i}/${MAX_ITER} of a gate loop. ${CD}` +
    `Gate: ${GATE}\n` +
    `Edit ONLY under: ${SCOPE.join(', ')}\n` +
    `Rules: ${RULES}\n${prior}\n` +
    `Protocol: run the gate and paste its tail; if exit 0 → status="green" and stop. Otherwise take the FIRST failure, read the code it names, ` +
    `make the minimal fix inside scope, re-run the gate; repeat within this iteration for at most 3 distinct failures, then report. ` +
    `Never widen scope; a failure outside scope goes into residual verbatim.`,
    { label: `gate:${i}`, phase: 'Gate', schema: GATE_SCHEMA, model: 'sonnet', effort: 'medium' }
  )
  if (!last) { last = { status: 'error', failuresBefore: 0, failuresAfter: 0, filesChanged: [], residual: ['agent returned no result'], summary: 'no result' } }
  history.push({ iteration: i, ...last })
  log(`gate ${i}/${MAX_ITER}: ${last.status} (${last.failuresBefore} → ${last.failuresAfter}) changed=${last.filesChanged.length}`)
  if (last.status === 'green') break
  if (last.status === 'red' && last.failuresBefore === last.failuresAfter && i > 1 && history[i - 2].failuresAfter === last.failuresAfter) {
    log('no progress across two iterations — escalating instead of burning the cap')
    break
  }
}
if (last.status !== 'green') log(`gate still ${last.status} after ${history.length} iteration(s); residual: ${last.residual.length}`)

return {
  status: last.status,
  iterations: history.length,
  filesChanged: [...new Set(history.flatMap(h => h.filesChanged))],
  residual: last.residual,
  history,
}
