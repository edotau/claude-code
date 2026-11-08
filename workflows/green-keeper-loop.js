export const meta = {
  name: 'green-keeper-loop',
  description: 'LoopAgent: run a gate command, fix what fails inside a scope, re-run — until green or maxIterations; default 3 iterations per call (verify.md\'s 5 is the whole-task cap)',
  whenToUse:
    'USE: make a gate pass: run it, fix failures inside scope, re-run (3 iterations per call by default). CALL: Workflow({scriptPath: "~/.claude/workflows/green-keeper-loop.js", args: {cwd, scope: ["internal/x"]}}). OPTIONAL: gate (auto-detected), maxIterations, fixesPerIteration, claudeState (ok|low|exhausted|unknown, passed by a caller; omit = read it here). RETURNS {status, exit: green | stalled | capped | error, gate, iterations, filesChanged, residual}. steps {detect, fix} = {agent?, model?, effort?, profile?, provider?} per stage (agent claude|codex|copilot|gemini|opencode; external stages run on the session\'s route, or args/steps profile|provider, and fall back on 429/auth; each reports ran on or BLOCKED). OPTIONAL: profile, provider (run-wide override for external stages).',
  phases: [
    { title: 'Detect', detail: 'haiku leaf picks the gate from Makefile/go.mod/pyproject/package.json/Cargo.toml when args.gate is absent' },
    { title: 'Gate', detail: 'one fixer per iteration: run the gate, fix root causes within scope, re-run' },
  ],
}

// ── Args ────────────────────────────────────────────────────────────────────
// Every field has a default so the workflow runs bare; scope is the ONLY place the fixer may edit.
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
// ── stepOf: copied verbatim into code-analyzer, green-keeper-loop, code-evaluator, code-workers, code-engineer ──
// stepOf(steps, name, defaults, {allowed, external, profile, provider}) -> stage; claude in-context, others via `agents ask`.
const COWORKERS = []
const stepOf = (steps, name, defaults, opt) => {
  const S_CLAUDE = ['haiku', 'sonnet', 'opus', 'fable']
  const S_AGENTS = ['claude', 'codex', 'copilot', 'gemini', 'opencode']
  const S_EFFORT = ['low', 'medium', 'high', 'xhigh', 'max']
  const S_FIELDS = ['agent', 'model', 'effort', 'profile', 'provider']
  const S_NAME = /^[A-Za-z0-9._-]+$/
  const o = opt || {}
  const allowed = o.allowed || []
  const d = defaults || {}
  if (steps !== undefined && (!steps || typeof steps !== 'object' || Array.isArray(steps))) throw new Error('steps must be an object')
  for (const k of Object.keys(steps || {})) if (!allowed.includes(k)) throw new Error(`steps.${k}: unknown stage (allowed: ${allowed.join('|')})`)
  const where = `steps.${name}`
  const v = (steps || {})[name] === undefined ? {} : steps[name]
  if (!v || typeof v !== 'object' || Array.isArray(v)) throw new Error(`${where} must be an object`)
  for (const k of Object.keys(v)) if (!S_FIELDS.includes(k)) throw new Error(`${where}.${k}: unknown field (allowed: ${S_FIELDS.join('|')})`)
  if (v.effort !== undefined && !S_EFFORT.includes(v.effort)) throw new Error(`${where}.effort must be ${S_EFFORT.join('|')}`)
  for (const k of ['model', 'profile', 'provider']) if (v[k] !== undefined && !S_NAME.test(String(v[k]))) throw new Error(`${where}.${k} must be a plain name (no provider: prefix)`)
  const who = v.agent !== undefined ? v.agent : 'claude'
  if (!S_AGENTS.includes(who)) throw new Error(`${where}.agent must be ${S_AGENTS.join('|')}`)
  const effort = v.effort !== undefined ? v.effort : d.effort || 'medium'
  if (who === 'claude') {
    if (v.profile !== undefined || v.provider !== undefined) throw new Error(`${where}: claude stages run in-context on the session profile; profile/provider need agent codex|copilot|gemini|opencode`)
    const model = v.model !== undefined ? v.model : d.model || 'sonnet'
    if (!S_CLAUDE.includes(model)) throw new Error(`${where}.model must be ${S_CLAUDE.join('|')}`)
    const same = (_label, r) => r
    return { agent: 'claude', ext: false, model, effort, type: d.agentType ? { agentType: d.agentType } : {}, task: (p) => p, schema: (s) => s, done: same, text: same }
  }
  if (!o.external) throw new Error(`${where}.agent: ${who} is not wired in this workflow yet (claude only)`)
  // gemini too: 'session' keeps its Google pin; a named profile or provider databricks runs it on Databricks (267d604e).
  const profile = v.profile !== undefined ? v.profile : o.profile || 'session'
  const provider = v.provider !== undefined ? v.provider : o.provider || ''
  const edit = d.tools === 'edit'
  const mode = edit ? '--tools' : who === 'copilot' ? '--read-only' : '--tools'
  const ask = `claude-code ${who} ask --fallback agents --format json ${mode} --effort ${effort} --profile ${profile}` +
    (provider ? ` --provider ${provider}` : '') + (v.model ? ` --model ${v.model}` : '')
  const F = { ran_on: { type: 'string' }, hops: { type: 'integer' }, coworker: { type: 'string', enum: ['ok', 'BLOCKED'] }, coworker_error: { type: 'string' } }
  const R = ['ran_on', 'coworker']
  const record = (label, r) => {
    const ranOn = (r && r.ran_on) || ''
    // the envelope's own line; a reworded one still counts when it names the roster agent that ran (logged)
    const ok = !!(r && r.coworker === 'ok' && (/^ran on \S/.test(ranOn) || /^(?:ran on )?(?:codex|copilot|gemini|opencode)\b\S*\s+\S/.test(ranOn)))
    if (ok && !/^ran on \S/.test(ranOn)) log(`coworkers ${label}: ran_on reworded by the relay: ${ranOn}`)
    const why = ok ? '' : (r && r.coworker_error) || 'no ran_on in the result'
    COWORKERS.push({ stage: name, label, agent: who, status: ok ? 'ok' : 'BLOCKED', ran_on: ok ? r.ran_on : '', hops: (r && r.hops) || 0, error: why })
    log(`coworkers ${label}: ${ok ? 'ran on ' + r.ran_on + ' (hop ' + ((r && r.hops) || 0) + ')' : who + ' BLOCKED: ' + why}`)
    return ok
  }
  return {
    agent: who, ext: true, model: 'sonnet', effort: 'low', type: { agentType: who }, profile, provider,
    task: (p) => `Agent: ${who}\nRun the task below with \`${ask} --attach <file>\`: Write the task text to a NEW file from \`mktemp "${'$'}{TMPDIR:-/tmp}/${who}-task.XXXXXX"\` (never a fixed name: parallel legs overwrite each other), never in the repo; attach that exact path, and never do it in-context.` +
      (edit ? '' : ' The task is read-only: tell the agent to edit nothing.') +
      ' If a tool call is refused because auto mode cannot judge it right now (its classifier is unavailable), retry that same call up to 3 times before reporting BLOCKED.' +
      ' The external agent does the whole task; never do it yourself or grade/answer in-context after a probe call. Copy the envelope\'s ran_on verbatim into ran_on, the length of its hops array in hops, and set coworker "ok". If every hop failed' +
      ' (429, auth, no route), set coworker "BLOCKED", coworker_error to the exact error, and the other fields to their empty values.\n\nTASK:\n' + p,
    schema: (s) => s ? { ...s, required: [...(s.required || []), ...R], properties: { ...s.properties, ...F } }
      : { type: 'object', additionalProperties: false, required: ['answer', ...R], properties: { answer: { type: 'string' }, ...F } },
    done: (label, r) => (record(label, r) ? r : null),
    text: (label, r) => (record(label, r) ? r.answer : null),
  }
}
// ── end stepOf ──
let ST
try {
  ST = {
    detect: stepOf(a.steps, 'detect', { model: 'haiku', effort: 'low' }, { allowed: ['detect', 'fix'], external: true, profile: a.profile, provider: a.provider }),
    fix: stepOf(a.steps, 'fix', { model: 'sonnet', effort: 'medium', tools: 'edit' }, { allowed: ['detect', 'fix'], external: true, profile: a.profile, provider: a.provider }),
  }
} catch (e) { return { status: 'error', exit: 'error', gate: null, gateSource: 'none', iterations: 0, filesChanged: [], residual: ['green-keeper-loop: ' + e.message], history: [] } }
for (const k of ['profile', 'provider']) if (a[k] !== undefined && !/^[A-Za-z0-9._-]+$/.test(String(a[k]))) return { status: 'error', exit: 'error', gate: null, gateSource: 'none', iterations: 0, filesChanged: [], residual: ['args.' + k + ' must be a plain name'], history: [] }
const EXT = [...new Map(Object.values(ST).filter((s) => s.ext).map((s) => [s.agent + '|' + s.profile + '|' + s.provider, s])).values()]
if (EXT.length) {
  const pf = await agent(
    'For each line, run it and report its candidate count and the first candidate as agent/account/model. Edit nothing.\n' +
      EXT.map((s) => `claude-code gateway router --agent ${s.agent} --fallback agents --profile ${s.profile}${s.provider ? ' --provider ' + s.provider : ''} --format json </dev/null`).join('\n'),
    { label: 'coworkers-preflight', phase: 'Preflight', model: 'haiku', effort: 'low', schema: { type: 'object', additionalProperties: false, required: ['routes'], properties: { routes: { type: 'array', items: { type: 'object', additionalProperties: false, required: ['agent', 'candidates', 'first'], properties: { agent: { type: 'string' }, candidates: { type: 'integer' }, first: { type: 'string' }, error: { type: 'string' } } } } } } },
  )
  for (const r of (pf && pf.routes) || []) log(`coworkers preflight ${r.agent}: ${r.candidates} route(s), first ${r.first}${r.error ? ' — ' + r.error : ''}`)
  const dead = ((pf && pf.routes) || []).filter((r) => r.candidates === 0).map((r) => r.agent + (r.error ? ' (' + r.error + ')' : ''))
  if (dead.length) return { status: 'error', exit: 'error', gate: null, gateSource: 'none', iterations: 0, filesChanged: [], residual: ['coworkers: no route for ' + dead.join(', ') + '; BLOCKED before any work'], history: [] }
}
let GATE = a.gate || ''
let GATE_SOURCE = a.gate ? 'args' : ''
const CD = a.cwd ? `cwd: ${a.cwd} (external agents: ask --workdir ${a.cwd}). Run each command bare, never as \`cd … && cmd\`: copilot's shell rejects compound commands.\n` : ''
const SCOPE_GIVEN = Array.isArray(a.scope) && a.scope.length > 0
const SCOPE = Array.isArray(a.scope) && a.scope.length ? a.scope : ['(whole repo — scope not given; prefer the smallest fix)']
const MAX_ITER = Number.isInteger(a.maxIterations) && a.maxIterations > 0 ? a.maxIterations : 3
const FIXES = Number.isInteger(a.fixesPerIteration) && a.fixesPerIteration > 0 ? a.fixesPerIteration : 3
const RULES = a.rules ||
  'Never run git stash|checkout|reset|restore|clean|commit; never run install/deploy/release targets or edit tool config. ' +
  'Edit only inside scope; re-run only the gate.'

// ── Pressure: Claude account state read once (by the caller, or here when absent); low/exhausted steps Claude down ──
const routeState = typeof a.claudeState === 'string' ? { state: a.claudeState } : await agent(
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
  let d = await agent(
    ST.detect.task(`${CD}Pick this repo's verify gate; do NOT run it. First call: ls Makefile go.mod pyproject.toml setup.cfg package.json Cargo.toml .venv 2>/dev/null; then read only the file that decides (at most 3 tool calls). ` +
    `The toolchain file decides; a Makefile only ever PREPENDS a lint step. Scope dirs: ${SCOPE.join(' ') || '(repo root)'}. ` +
    `(1) go.mod → "go build ./... && go vet ./<dir>/... && go test ./<dir>/..." over the scope dirs (./... when the scope is the root); ` +
    `(2) pyproject.toml/setup.cfg → "ruff check <dir> && pytest -q <dir-or-tests>" (ruff only if configured; prefix each with .venv/bin/python -m when .venv exists); ` +
    `(3) package.json → "npm run lint && npm test -- <scope paths>" using only scripts that exist; (4) Cargo.toml → "cargo build && cargo test -p <crate of each scope dir>". ` +
    `If a Makefile has a lint (or fmt/vet) target with a report-only form (e.g. its help says CHECK=1 reports), return "make lint CHECK=1 && " + that command and source "Makefile"; ` +
    `skip a lint target that only auto-fixes — it edits files outside the scope, and other sessions share this checkout. Never return make test/audit/check/install/deploy/release/e2e — a suite-wide target is not a scoped gate. ` +
    `(5) Makefile only, no toolchain file, or no scoped command derivable → gate "" and source "none" (the caller must pass args.gate).`),
    { label: 'detect', phase: 'Detect', schema: ST.detect.schema(DETECT_SCHEMA), ...ST.detect.type, model: routeModel(ST.detect.model), effort: ST.detect.effort }
  )
  d = ST.detect.done('detect', d)
  GATE = (d && d.gate) || ''
  GATE_SOURCE = (d && d.source) || 'none'
  log(`gate detected (${GATE_SOURCE}): ${GATE || 'none'}`)
  if (!GATE) return { status: 'error', exit: 'error', gate: null, gateSource: GATE_SOURCE, iterations: 0, filesChanged: [], residual: ['no gate detected under ' + (a.cwd || 'cwd') + '; pass args.gate'], history: [] }
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
    `A failure outside scope goes into residual verbatim. If the gate cannot run at all (permission denied, missing command), set status="error", residual to the exact refusal, and stop.`
  last = await agent(
    ST.fix.task(prompt),
    { label: `gate:${i}`, phase: 'Gate', schema: ST.fix.schema(GATE_SCHEMA), ...ST.fix.type, model: routeModel(ST.fix.model), effort: ST.fix.effort }
  )
  last = ST.fix.done('gate:' + i, last)
  if (!last) { last = { status: 'error', failuresBefore: 0, failuresAfter: 0, filesChanged: [], residual: ['agent returned no result'], summary: 'no result' } }
  history.push({ iteration: i, ...last })
  log(`gate ${i}/${MAX_ITER}: ${last.status} (${last.failuresBefore} → ${last.failuresAfter}) changed=${last.filesChanged.length}`)
  if (last.status === 'green' || last.status === 'error') break // an unrunnable gate never improves by retrying
  if (i > 1 && last.status !== 'green' && last.status === history[i - 2].status && last.failuresBefore === last.failuresAfter && history[i - 2].failuresAfter === last.failuresAfter) {
    stalled = true
    log('no progress across two iterations — escalating instead of burning the cap')
    break
  }
}
if (last.status !== 'green') log(`gate still ${last.status} after ${history.length} iteration(s); residual: ${last.residual.length}`)

return {
  status: last.status,
  exit: last.status === 'green' ? 'green' : last.status === 'error' ? 'error' : stalled ? 'stalled' : 'capped',
  gate: GATE,
  gateSource: GATE_SOURCE,
  iterations: history.length,
  filesChanged: [...new Set(history.flatMap(h => h.filesChanged))],
  residual: last.residual,
  history,
  coworkers: COWORKERS,
}
