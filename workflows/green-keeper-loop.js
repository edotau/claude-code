export const meta = {
  name: 'green-keeper-loop',
  description: 'LoopAgent: run a gate command, fix what fails inside a scope, re-run — until green or maxIterations; default 3 iterations per call (verify.md\'s 5 is the whole-task cap)',
  whenToUse:
    'USE: make a gate pass: run it, fix failures inside scope, re-run (3 iterations per call by default). CALL: Workflow({scriptPath: "~/.claude/workflows/green-keeper-loop.js", args: {cwd, scope: ["internal/x"]}}). OPTIONAL: gate (auto-detected), maxIterations, fixesPerIteration. RETURNS {status, exit: green | stalled | capped | error, gate, iterations, filesChanged, residual}. steps {detect, fix}: each {agent, model, effort, profile}; detect is claude only, fix may be external (codex|gemini|opencode|copilot, model = vendor id) at top level only.',
  phases: [
    { title: 'Detect', detail: 'haiku leaf picks the gate from Makefile/go.mod/pyproject/package.json/Cargo.toml when args.gate is absent' },
    { title: 'Gate', detail: 'one fixer per iteration: run the gate, fix root causes within scope, re-run' },
  ],
}

// ── Args ────────────────────────────────────────────────────────────────────
// Every field has a default so the workflow runs bare; scope is the ONLY place the fixer may edit.
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
// ── stepOf: copied verbatim into code-analyzer, code-engineer, green-keeper-loop, code-evaluator ──
// stepOf(steps, name, defaults, {allowed, externalOK, lists}) -> {kind:'claude',model,effort} | {kind:'external',agent,vendorModel,profile}
// An array value (only for stages named in `lists`) returns an array of those; any bad input throws Error naming stage/field.
const stepOf = (steps, name, defaults, opt) => {
  const S_AGENTS = ['claude', 'copilot', 'codex', 'gemini', 'opencode']
  const S_CLAUDE = ['haiku', 'sonnet', 'opus']
  const S_EFFORT = ['low', 'medium', 'high', 'xhigh']
  const S_FIELDS = ['agent', 'model', 'effort', 'profile']
  const allowed = (opt && opt.allowed) || []
  const lists = (opt && opt.lists) || []
  const d = defaults || {}
  if (steps !== undefined && (!steps || typeof steps !== 'object' || Array.isArray(steps))) throw new Error('steps must be an object')
  for (const k of Object.keys(steps || {})) if (!allowed.includes(k)) throw new Error(`steps.${k}: unknown stage (allowed: ${allowed.join('|')})`)
  const one = (v, where) => {
    if (!v || typeof v !== 'object' || Array.isArray(v)) throw new Error(`${where} must be an object`)
    for (const k of Object.keys(v)) if (!S_FIELDS.includes(k)) throw new Error(`${where}.${k}: unknown field (allowed: ${S_FIELDS.join('|')})`)
    const agentName = v.agent !== undefined ? v.agent : d.agent || 'claude'
    if (!S_AGENTS.includes(agentName)) throw new Error(`${where}.agent must be ${S_AGENTS.join('|')}`)
    if (v.effort !== undefined && !S_EFFORT.includes(v.effort)) throw new Error(`${where}.effort must be ${S_EFFORT.join('|')}`)
    if (v.profile !== undefined && !/^[A-Za-z0-9._-]+$/.test(String(v.profile))) throw new Error(`${where}.profile must be a profile name`)
    const effort = v.effort !== undefined ? v.effort : d.effort || 'medium'
    if (agentName === 'claude') {
      const model = v.model !== undefined ? v.model : d.agent && d.agent !== 'claude' ? 'sonnet' : d.model || 'sonnet'
      if (!S_CLAUDE.includes(model)) throw new Error(`${where}.model must be ${S_CLAUDE.join('|')} for agent claude`)
      return { kind: 'claude', model, effort }
    }
    if (!(opt && opt.externalOK)) throw new Error(`${where}.agent: ${agentName} not allowed here (claude only)`)
    const vendorModel = v.model !== undefined ? v.model : v.agent === undefined ? d.model : undefined
    if (vendorModel !== undefined && !/^[A-Za-z0-9._:-]+$/.test(String(vendorModel))) throw new Error(`${where}.model must be a model id or alias`)
    return { kind: 'external', agent: agentName, vendorModel, profile: v.profile !== undefined ? v.profile : d.profile }
  }
  const v = (steps || {})[name]
  if (Array.isArray(v)) {
    if (!lists.includes(name)) throw new Error(`steps.${name}: a list is not allowed for this stage`)
    if (!v.length) throw new Error(`steps.${name}: list is empty`)
    return v.map((x, i) => one(x, `steps.${name}[${i}]`))
  }
  return one(v === undefined ? {} : v, `steps.${name}`)
}
// ── end stepOf ──
let ST
try {
  ST = {
    detect: stepOf(a.steps, 'detect', { model: 'haiku', effort: 'low' }, { allowed: ['detect', 'fix'] }),
    fix: stepOf(a.steps, 'fix', { model: 'sonnet', effort: 'medium' }, { allowed: ['detect', 'fix'], externalOK: true }),
  }
} catch (e) { return { status: 'error', exit: 'error', gate: null, gateSource: 'none', iterations: 0, filesChanged: [], residual: ['green-keeper-loop: ' + e.message], history: [] } }
let GATE = a.gate || ''
let GATE_SOURCE = a.gate ? 'args' : ''
const CD = a.cwd ? `cwd: ${a.cwd}. Prefix every command with \`cd ${a.cwd} &&\` (the shell cwd resets).\n` : ''
const SCOPE_GIVEN = Array.isArray(a.scope) && a.scope.length > 0
const SCOPE = Array.isArray(a.scope) && a.scope.length ? a.scope : ['(whole repo — scope not given; prefer the smallest fix)']
const MAX_ITER = Number.isInteger(a.maxIterations) && a.maxIterations > 0 ? a.maxIterations : 3
const FIXES = Number.isInteger(a.fixesPerIteration) && a.fixesPerIteration > 0 ? a.fixesPerIteration : 3
const RULES = a.rules ||
  'Never run git stash|checkout|reset|restore|clean|commit; never run install/deploy/release targets or edit tool config. ' +
  'Edit only inside scope; re-run only the gate.'

// ── Pressure: Claude account state read once; low/exhausted steps every Claude stage down one model ──
const routeState = await agent(
  `Run exactly: ${a.cwd ? 'cd ' + a.cwd + ' && ' : ''}claude-code gateway router --format json --agent claude --fallback none --model sonnet. ` +
    `It prints one JSON object; state = candidates[0].state when candidates is non-empty, else the skipped[] entry whose agent ` +
    `is "claude" and its gate value, else "unknown". Never run anything else.`,
  { label: 'route-state', phase: 'Detect', schema: { type: 'object', additionalProperties: false, required: ['state'], properties: { state: { type: 'string' } } }, model: 'haiku', effort: 'low' }
).catch(() => null)
const MODEL_DOWN = { opus: 'sonnet', sonnet: 'haiku', haiku: 'haiku' }
const PRESSURE = !!(routeState && (routeState.state === 'low' || routeState.state === 'exhausted'))
const routeModel = m => (PRESSURE && MODEL_DOWN[m]) ? MODEL_DOWN[m] : m
if (PRESSURE) log(`pressure: claude account ${routeState.state} — stepping Claude stages down one model`)
else if (!routeState) log('pressure: route read failed — no downshift (fail open)')

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

const DETECT_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['gate', 'source'],
  properties: {
    gate: { type: 'string', description: 'exact command, empty when nothing matches' },
    source: { type: 'string', enum: ['Makefile', 'go.mod', 'pyproject', 'package.json', 'Cargo.toml', 'none'] },
  },
}

// ── Detect (ADK leaf: pick the verify gate when the caller didn't pass one) ─────────────────
if (!GATE && !SCOPE_GIVEN) {
  return { status: 'error', exit: 'error', gate: null, gateSource: 'none', iterations: 0, filesChanged: [], residual: ['no scope and no gate: pass args.gate or args.scope; a suite-wide gate is never auto-detected'], history: [] }
}
if (!GATE) {
  phase('Detect')
  const d = await agent(
    `${CD}Pick this repo's verify gate; do NOT run it. First call: ls Makefile go.mod pyproject.toml setup.cfg package.json Cargo.toml .venv 2>/dev/null; then read only the file that decides (at most 3 tool calls). ` +
    `The toolchain file decides; a Makefile only ever PREPENDS a lint step. Scope dirs: ${SCOPE.join(' ') || '(repo root)'}. ` +
    `(1) go.mod → "go build ./... && go vet ./<dir>/... && go test ./<dir>/..." over the scope dirs (./... when the scope is the root); ` +
    `(2) pyproject.toml/setup.cfg → "ruff check <dir> && pytest -q <dir-or-tests>" (ruff only if configured; prefix each with .venv/bin/python -m when .venv exists); ` +
    `(3) package.json → "npm run lint && npm test -- <scope paths>" using only scripts that exist; (4) Cargo.toml → "cargo build && cargo test -p <crate of each scope dir>". ` +
    `If a Makefile has a lint (or fmt/vet) target, return "make lint && " + that command and source "Makefile"; never return make test/audit/check/install/deploy/release/e2e — a suite-wide target is not a scoped gate. ` +
    `(5) Makefile only, no toolchain file, or no scoped command derivable → gate "" and source "none" (the caller must pass args.gate).`,
    { label: 'detect', phase: 'Detect', schema: DETECT_SCHEMA, model: routeModel(ST.detect.model), effort: ST.detect.effort }
  )
  GATE = (d && d.gate) || ''
  GATE_SOURCE = (d && d.source) || 'none'
  log(`gate detected (${GATE_SOURCE}): ${GATE || 'none'}`)
  if (!GATE) return { status: 'error', exit: 'error', gate: null, gateSource: GATE_SOURCE, iterations: 0, filesChanged: [], residual: ['no gate detected under ' + (a.cwd || 'cwd') + '; pass args.gate'], history: [] }
}

// External fixer: code-agent edits in place and replies text; the JSON line it ends with maps onto GATE_SCHEMA.
const viaExternal = async (step, prompt) => {
  const r = await workflow('code-agent', { agent: step.agent, vendorModel: step.vendorModel, profile: step.profile, cwd: a.cwd, tools: true, fallback: false,
    task: prompt + '\nEnd your reply with ONE JSON line: {"status":"green|red|error","failuresBefore":n,"failuresAfter":n,"filesChanged":[],"residual":[],"summary":""}' })
  const m = r && r.ok && typeof r.answer === 'string' ? r.answer.match(/\{[^{}]*"status"[^{}]*\}\s*$/) : null
  try { const o = JSON.parse(m[0]); return GATE_SCHEMA.required.every(k => k in o) ? o : null } catch { return null }
}

// ── Loop (ADK LoopAgent: sub-agent per iteration, exit on the escalation signal or the cap) ────
phase('Gate')
const history = []
let last = null
let stalled = false
for (let i = 1; i <= MAX_ITER; i++) {
  const prior = last && last.residual.length ? `\nStill red after iteration ${i - 1}:\n- ${last.residual.join('\n- ')}\n` : ''
  const prompt =
    `Iteration ${i}/${MAX_ITER} of a gate loop. ${CD}` +
    `Method: ~/.claude/skills/problem-solving/references/verify.md (Gate Loop)\n` +
    `Gate: ${GATE}\n` +
    `Edit ONLY under: ${SCOPE.join(', ')}\n` +
    `Rules: ${RULES}\n${prior}\n` +
    `Protocol: run the gate and paste its tail; if exit 0 → status="green" and stop. Otherwise fix the FIRST failure per the Method, re-run the gate; ` +
    `repeat for at most ${FIXES} distinct failures this iteration, then report (the rest go into residual verbatim). ` +
    `A failure outside scope goes into residual verbatim.`
  last = ST.fix.kind === 'external' ? await viaExternal(ST.fix, prompt) : await agent(
    prompt,
    { label: `gate:${i}`, phase: 'Gate', schema: GATE_SCHEMA, model: routeModel(ST.fix.model), effort: ST.fix.effort }
  )
  if (!last) { last = { status: 'error', failuresBefore: 0, failuresAfter: 0, filesChanged: [], residual: ['agent returned no result'], summary: 'no result' } }
  history.push({ iteration: i, ...last })
  log(`gate ${i}/${MAX_ITER}: ${last.status} (${last.failuresBefore} → ${last.failuresAfter}) changed=${last.filesChanged.length}`)
  if (last.status === 'green') break
  if (i > 1 && last.status !== 'green' && last.status === history[i - 2].status && last.failuresBefore === last.failuresAfter && history[i - 2].failuresAfter === last.failuresAfter) {
    stalled = true
    log('no progress across two iterations — escalating instead of burning the cap')
    break
  }
}
if (last.status !== 'green') log(`gate still ${last.status} after ${history.length} iteration(s); residual: ${last.residual.length}`)

return {
  status: last.status,
  exit: last.status === 'green' ? 'green' : stalled ? 'stalled' : 'capped', // 'error' normalizes upstream; stall/cap always close the loop first
  gate: GATE,
  gateSource: GATE_SOURCE,
  iterations: history.length,
  filesChanged: [...new Set(history.flatMap(h => h.filesChanged))],
  residual: last.residual,
  history,
}
