export const meta = {
  name: 'dag',
  description: 'DAG runner: validate a step graph, then start each step the moment all its deps settle; skips propagate through, failures skip dependents',
  whenToUse:
    'A task whose sub-agents form a dependency graph rather than a straight pipeline (port of the harness triggerer dag.go). Usually called inline: workflow("dag", {steps: [...]}). args: {steps: [{name, prompt, deps?: [names], model? | agentType?, effort: "low|medium|high|xhigh|max", schema?, skip?: true, when?: {step, equals}}], maxConcurrent?}. Prompts may reference a dep result as {{depName}}; when.step must be a dep. Returns {order, results, skipped, failed} or {error} before any agent spawns.',
  phases: [
    { title: 'Validate', detail: 'names unique, deps exist, no cycles, every step pinned (model, plus effort; agentType optional), templates reference deps only' },
    { title: 'Steps', detail: 'one phase per step name; event-driven: a step starts as soon as all of its deps settle, no barrier waves' },
  ],
}

// ── Args ────────────────────────────────────────────────────────────────────
// The registry imports the script to read its meta, so bad args return {error}, never throw.
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const STEPS = Array.isArray(a.steps) ? a.steps : []
if (!STEPS.length) return { error: 'dag needs args.steps: [{name, prompt, deps?, model, agentType?, effort}]' }
const MAX_CONC = Number.isInteger(a.maxConcurrent) && a.maxConcurrent > 0 ? a.maxConcurrent : STEPS.length
const EFFORTS = ['low', 'medium', 'high', 'xhigh', 'max']
const TEMPLATE = /\{\{\s*([^{}]+?)\s*\}\}/g

// ── Validate (before any agent spawns) ──────────────────────────────────────
const validate = () => {
  const byName = new Map()
  for (const [i, s] of STEPS.entries()) {
    if (!s || typeof s !== 'object') return `step #${i} is not an object`
    if (typeof s.name !== 'string' || !s.name.trim()) return `step #${i} has no name`
    if (byName.has(s.name)) return `duplicate step name "${s.name}"`
    byName.set(s.name, s)
  }
  for (const s of STEPS) {
    const deps = s.deps === undefined ? [] : s.deps
    if (!Array.isArray(deps)) return `step "${s.name}": deps must be an array of step names`
    for (const d of deps) if (!byName.has(d)) return `step "${s.name}" depends on unknown step "${d}"`
    if (typeof s.prompt !== 'string' || !s.prompt.trim()) return `step "${s.name}" has no prompt`
    if (!s.model) return `step "${s.name}" pins no model (agentType alone keeps a built-in\'s own tier; same discipline as workflow lint)`
    if (!EFFORTS.includes(s.effort)) return `step "${s.name}" effort must be one of ${EFFORTS.join('|')}, got ${JSON.stringify(s.effort)}`
    for (const m of s.prompt.matchAll(TEMPLATE)) {
      if (!deps.includes(m[1])) return `step "${s.name}" prompt references {{${m[1]}}}, which is not one of its deps`
    }
    if (s.when !== undefined) {
      if (!s.when || typeof s.when !== 'object' || !('equals' in s.when)) return `step "${s.name}": when must be {step, equals}`
      if (!deps.includes(s.when.step)) return `step "${s.name}": when.step "${s.when.step}" must be one of its deps`
    }
  }
  // DFS with visited + recursion stack; post-order doubles as a topological order.
  const visited = new Set()
  const stack = []
  const onStack = new Set()
  const topo = []
  const visit = n => {
    if (onStack.has(n)) return `cycle: ${[...stack.slice(stack.indexOf(n)), n].join(' -> ')}`
    if (visited.has(n)) return null
    visited.add(n); onStack.add(n); stack.push(n)
    for (const d of byName.get(n).deps || []) {
      const err = visit(d)
      if (err) return err
    }
    stack.pop(); onStack.delete(n); topo.push(n)
    return null
  }
  for (const s of STEPS) {
    const err = visit(s.name)
    if (err) return err
  }
  return { byName, topo }
}
const v = validate()
if (typeof v === 'string') { log(`dag: invalid — ${v}`); return { error: v } }
const { byName, topo } = v

// ── Run ─────────────────────────────────────────────────────────────────────
let running = 0
const waiters = []
const acquire = () => running < MAX_CONC ? (running++, Promise.resolve()) : new Promise(r => waiters.push(r))
const release = () => { const w = waiters.shift(); if (w) w(); else running-- }

const asText = r => r === null || r === undefined ? '' : typeof r === 'string' ? r : JSON.stringify(r)
const state = new Map() // name → {status: 'ok'|'skipped'|'failed', result}
const order = []
const results = {}
const skipped = []
const failed = []
const settle = (name, status, result) => {
  state.set(name, { status, result })
  order.push(name)
  if (status === 'ok') results[name] = result
  if (status === 'skipped') skipped.push(name)
  if (status === 'failed') failed.push(name)
}
// Effective upstream: walk past skipped deps to their own deps (dag.go Dependencies skips Skip vertices).
const effectiveDeps = (name, seen = new Set()) => {
  const out = []
  for (const d of byName.get(name).deps || []) {
    if (seen.has(d)) continue
    seen.add(d)
    if (state.get(d).status === 'skipped') out.push(...effectiveDeps(d, seen))
    else out.push(d)
  }
  return out
}

const runStep = async s => {
  const name = s.name
  const badDeps = effectiveDeps(name).filter(d => state.get(d).status === 'failed')
  let why = s.skip ? 'skip:true' : badDeps.length ? `dep failed: ${badDeps.join(', ')}` : ''
  if (!why && s.when) {
    const got = asText(state.get(s.when.step).result).trim()
    if (got !== String(s.when.equals)) why = `when ${s.when.step}=${JSON.stringify(got.slice(0, 80))} ≠ ${JSON.stringify(String(s.when.equals))}`
  }
  if (why) { log(`skip ${name} (${why})`); return settle(name, 'skipped', null) }

  // A skipped dep contributes no output, so its placeholder renders empty.
  const prompt = s.prompt.replace(TEMPLATE, (_, d) => asText(state.get(d).result))
  await acquire()
  log(`start ${name}`)
  let out = null
  try {
    // Pins are per-step variables; validate() already enforced model and effort on every step.
    out = await agent(prompt, { label: name, phase: name, schema: s.schema, model: s.model, agentType: s.agentType, effort: s.effort })
  } catch (e) {
    log(`fail ${name}: ${e && e.message ? e.message : e}`)
  } finally {
    release()
  }
  if (out === null || out === undefined) { log(`fail ${name} (agent returned no result)`); return settle(name, 'failed', null) }
  log(`done ${name}`)
  return settle(name, 'ok', out)
}

// Promises are created in topo order, so every dep's promise exists before a dependent awaits it.
const done = new Map()
for (const n of topo) {
  const s = byName.get(n)
  done.set(n, Promise.all((s.deps || []).map(d => done.get(d))).then(() => runStep(s)))
}
await Promise.all(done.values())
log(`dag: ${Object.keys(results).length} ok, ${skipped.length} skipped, ${failed.length} failed`)

return { order, results, skipped, failed }
