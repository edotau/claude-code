export const meta = {
  name: 'code-looper',
  description: 'LoopAgent: run a gate command, fix what fails inside a scope, re-run — until green or maxIterations; default 3 iterations per call (verify.md\'s 5 is the whole-task cap)',
  whenToUse: 'USE: make a scoped gate pass with bounded parallel fixes. CALL: Workflow code-looper with scope or files. RETURNS: exit, gateRuns, residual and iterationsUsed.',
  phases: [
    { title: 'Detect', detail: 'haiku leaf picks the gate from Makefile/go.mod/pyproject/package.json/Cargo.toml when args.gate is absent' },
    { title: 'Gate', detail: 'a read-only check first; then one fixer per iteration: run the gate, fix root causes within scope, re-run; a fixer green is re-checked' },
  ],
}

// ── Args ────────────────────────────────────────────────────────────────────
// Scope or files bounds every fixer; an explicit gate does not grant repository-wide edits.
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
// ── stepOf: copied verbatim into code-analyzer, code-looper, code-evaluator, code-workers, code-engineer ──
// stepOf(steps, name, defaults, {allowed, external, profile, provider}) -> stage; claude in-context, others via `agents ask`.
const COWORKERS = []
// ranOnReworded: a relay-reworded ran_on that still names the agent and a route (account·profile, profile:model or model id).
const ranOnReworded = (ranOn) => /^(?:ran on )?(?:claude|codex|copilot|gemini|opencode)\b/.test(ranOn) && (/\S[·:][A-Za-z0-9]/.test(ranOn) || /\b[A-Za-z]+-[\w.-]*\d/.test(ranOn))
// ranOnVerdict reads a leg's result: ranOn as relayed, whether it is the envelope's line (canon, wire.RanOn) and whether the leg ran.
const ranOnVerdict = (r) => {
  const ranOn = (r && r.ran_on) || ''
  const canon = /^ran on \S+ · .+ · fallback (?:none|from )/.test(ranOn)
  const failed = /\b(?:unavailable|failed|blocked|no execution|not run)\b/i.test(ranOn)
  return { ranOn, canon, ok: !!(r && r.coworker === 'ok' && (canon || ranOnReworded(ranOn)) && !failed) }
}
// Unknown stage/field backstop only; value and route checks run in the dispatch-effort hook (workflowlint.go).
const S_FIELDS = ['agent', 'model', 'effort', 'profile', 'provider']
const stepObj = (v) => !!v && typeof v === 'object' && !Array.isArray(v)
const stepUnknown = (obj, allowed, msg) => { for (const k of Object.keys(obj)) if (!allowed.includes(k)) throw new Error(msg(k)) }
const stepCheck = (steps, name, allowed) => {
  if (steps !== undefined && !stepObj(steps)) throw new Error('steps must be an object')
  const all = steps || {}, al = allowed || []
  stepUnknown(all, al, (k) => `steps.${k}: unknown stage (allowed: ${al.join('|')})`)
  const where = `steps.${name}`
  const v = all[name] === undefined ? {} : all[name]
  if (!stepObj(v)) throw new Error(`${where} must be an object`)
  stepUnknown(v, S_FIELDS, (k) => `${where}.${k}: unknown field (allowed: ${S_FIELDS.join('|')})`)
  return v
}
const stepFields = (v, d, o) => ({ who: v.agent !== undefined ? v.agent : 'claude', effort: v.effort !== undefined ? v.effort : d.effort || 'medium',
  profile: v.profile !== undefined ? v.profile : o.profile || '', provider: v.provider !== undefined ? v.provider : o.provider || '' })
const stepSame = (_label, r) => r
const claudeStage = (v, d, effort) => ({ agent: 'claude', ext: false, model: v.model !== undefined ? v.model : d.model || 'sonnet', effort,
  type: d.agentType ? { agentType: d.agentType } : {}, task: (p) => p, schema: (s) => s, done: stepSame, text: stepSame })
const ST_EXT_RETRY = ' If a tool call is refused because auto mode cannot judge it right now (its classifier is unavailable), retry that same call up to 3 times before reporting BLOCKED.'
const ST_EXT_DONE = ' Copy the envelope dispatch id into dispatch_id and run `claude-code agents diff --verify <id>` with that id; verification failure means BLOCKED. The external agent does the whole task; never do it yourself or grade/answer in-context after a probe call. Copy the envelope\'s ran_on verbatim into ran_on, the length of its hops array in hops, and set coworker "ok". If every hop failed'
const ST_EXT_FAIL = ' (429, auth, no route), set coworker "BLOCKED", coworker_error to the exact error, and the other fields to their empty values.\n\nTASK:\n'
const EXT_F = { dispatch_id: { type: 'string' }, ran_on: { type: 'string' }, hops: { type: 'integer' }, coworker: { type: 'string', enum: ['ok', 'BLOCKED'] }, coworker_error: { type: 'string' } }
const EXT_R = ['dispatch_id', 'ran_on', 'coworker']
const stepRecord = (name, who, label, r) => {
  const { ranOn, canon, ok } = ranOnVerdict(r)
  if (ok && !canon) log(`coworkers ${label}: ran_on reworded by the relay: ${ranOn}`)
  const why = ok ? '' : (r && r.coworker_error) || 'no ran_on in the result'
  COWORKERS.push({ stage: name, label, agent: who, status: ok ? 'ok' : 'BLOCKED', ran_on: ok ? r.ran_on : '', hops: (r && r.hops) || 0, error: why })
  log(`coworkers ${label}: ${ok ? (/^ran on /.test(r.ran_on) ? '' : 'ran on ') + r.ran_on + ' (hop ' + ((r && r.hops) || 0) + ')' : who + ' BLOCKED: ' + why}`)
  return ok
}
const stepExt = (name, v, d, f) => {
  const { who, effort, profile, provider } = f
  // no profile/provider: Go routes the agent's pin; a named one leads, ladder after.
  const edit = d.tools === 'edit'
  const mode = edit || d.tools === 'run' ? '--tools' : who === 'copilot' ? '--read-only' : '--tools'
  const ask = `claude-code agents ask --agent ${who} --fallback agents --format json ${mode} --effort ${effort}` + (profile ? ` --profile ${profile}` : '') +
    (provider ? ` --provider ${provider}` : '') + (v.model ? ` --model ${v.model}` : '')
  return {
    agent: who, ext: true, model: 'sonnet', effort: 'low', type: { agentType: who }, profile, provider,
    task: (p) => `Agent: ${who}\nRun the task below with \`${ask} --attach <file>\`: Write the task text to a NEW file from \`mktemp "${'$'}{TMPDIR:-/tmp}/${who}-task.XXXXXX"\` (never a fixed name: parallel legs overwrite each other), never in the repo; Read that empty file before you Write it (Write refuses an existing file it has not read), attach that exact path, and never do it in-context.` +
      (edit ? '' : ' The task is read-only: tell the agent to edit nothing.') + ST_EXT_RETRY + ST_EXT_DONE + ST_EXT_FAIL + p,
    schema: (s) => s ? { ...s, required: [...(s.required || []), ...EXT_R], properties: { ...s.properties, ...EXT_F } }
      : { type: 'object', additionalProperties: false, required: ['answer', ...EXT_R], properties: { answer: { type: 'string' }, ...EXT_F } },
    done: (label, r) => (stepRecord(name, who, label, r) ? r : null),
    text: (label, r) => (stepRecord(name, who, label, r) ? r.answer : null),
  }
}
const stepOf = (steps, name, defaults, opt) => {
  const o = opt || {}, d = defaults || {}
  const v = stepCheck(steps, name, o.allowed)
  const f = stepFields(v, d, o)
  if (f.who === 'claude' && !f.profile && !f.provider) return claudeStage(v, d, f.effort)
  if (!o.external) throw new Error(`steps.${name}.agent: ${f.who} is not wired in this workflow yet (claude only)`)
  return stepExt(name, v, d, f)
}
// ── end stepOf ──
// ── stages: copied verbatim into code-analyzer, code-looper, code-workers, code-engineer ──
// Shared stages, run in-script so workflow() nests one level; ctx = {cwd, cd, rules, S, model?, gateMax?, gate?, kind?, lineBudget?, seams?}.
const ST_NEAR = 10
const ST_GATE_MAX = 2
const stRel = (ctx, f) => String(f || '').replace(ctx.cwd ? ctx.cwd + '/' : /^$/, '').replace(/^\.\//, '')
const stDir = (f) => String(f).split('/').slice(0, -1).join('/') || '.'
// stDiff: the brief's diff command, in the spelling copilot's shell allows; it names untracked new files too.
const stDiff = (ps) => '`git diff --no-ext-diff --no-textconv -- ' + ps.join(' ') + '` plus every new (`??`) file `git status --porcelain -- ' + ps.join(' ') + '` lists'
// sameBug: one bug when two findings share a file and sit within ST_NEAR lines or name the same symbol.
const sameBug = (ctx, g, f) => stRel(ctx, g.file) === stRel(ctx, f.file) && (Math.abs((g.line || 0) - (f.line || 0)) <= ST_NEAR || (!!g.symbol && g.symbol === f.symbol))
const lowBudget = () => typeof budget !== 'undefined' && !!budget && !!budget.total && budget.remaining() < 150000
// stCost: a cost finding (perf, runtime, complexity, test-quality) has no failing scenario, only a cited cost and a safe fix.
const stCost = (f) => !!f.kind && f.kind !== 'bug'
const ST_LIST = { type: 'array', items: { type: 'string' } }
const ST_WORK = { type: 'object', properties: { status: { type: 'string', enum: ['done', 'partial', 'blocked'] }, files: ST_LIST, summary: { type: 'string' }, callers: { type: 'string' }, leftUndone: ST_LIST }, required: ['status', 'files', 'summary'] }
const ST_FAILURES = { type: 'array', items: { type: 'object', properties: { file: { type: 'string' }, line: { type: 'integer' }, text: { type: 'string' } }, required: ['file', 'line', 'text'] } }
const ST_CHECK = { type: 'object', properties: { gate: { type: 'string' }, state: { type: 'string' }, exit: { type: ['integer', 'null'] }, changed: ST_LIST, failures: ST_FAILURES }, required: ['gate', 'exit', 'changed', 'failures'] }
const ST_SIMP = { type: 'object', properties: { applied: ST_LIST, flagged: ST_LIST, deltaLines: { type: 'integer' }, status: { type: 'string', enum: ['done', 'clean', 'partial', 'blocked'] } }, required: ['applied', 'flagged', 'deltaLines', 'status'] }
const ST_REVF = { type: 'object', properties: { findings: { type: 'array', items: { type: 'object', properties: { file: { type: 'string' }, line: { type: 'integer' }, symbol: { type: 'string' }, symptom: { type: 'string' }, severity: { type: 'string', enum: ['critical', 'high', 'medium', 'low'] } }, required: ['file', 'line', 'symbol', 'symptom', 'severity'] } } }, required: ['findings'] }
const ST_DETECT = { type: 'object', additionalProperties: false, required: ['gate', 'source'],
  properties: { gate: { type: 'string', description: 'exact command, empty when nothing matches' }, source: { type: 'string' } } }
// Measured run state: numstat rows per editing stage, proposed tests, over-budget fixes, seam needs, gate results in order.
const ST_LINES = [], ST_PROPOSED = [], ST_OVER = [], ST_SEAMS = [], ST_GATES = []
const ST_TEST_RE = /(_test\.go$|\.test\.[jt]sx?$|(^|\/)(tests|testdata)\/)/
const ST_BUDGET = { shrink: 0, neutral: 20 } // max production net lines per package; free has no entry
const stOver = (ctx, net) => ST_BUDGET[ctx.lineBudget] !== undefined && net > ST_BUDGET[ctx.lineBudget]
const ST_FIXW = { ...ST_WORK, properties: { ...ST_WORK.properties, seams: { type: 'array', items: { type: 'object', properties: { file: { type: 'string' }, need: { type: 'string' } }, required: ['file', 'need'] } } } }
const ST_TESTW = { ...ST_WORK, properties: { ...ST_WORK.properties, proposed: { type: 'array', items: { type: 'object', properties: { case: { type: 'string' }, reason: { type: 'string' }, runtime: { type: 'string' } }, required: ['case', 'reason', 'runtime'] } } } }
const ST_PROPOSE = ' Every other test worth adding (a regression test for a bug fix, a pin for a cost fix) stays unwritten: put it in `proposed` as {case (input → expected), reason (what it guards), runtime (expected, e.g. "<5ms")}.' +
  ' Any test you write must run in milliseconds: no sleep, real network, vault, daemon or spawned binary; each under complexity 10.'
// the numstat reader is always a local cheap leaf: an external relay adds a hop and a second self-report
const ST_MEASURER = { agent: 'claude', ext: false, model: 'haiku', effort: 'low', type: {}, task: (p) => p, schema: (s) => s, done: (_l, r) => r }
// stSnap: observed counts from agents diff for files under paths (its `outside` peer files dropped); null on failure.
async function stSnap(ctx, label, paths) {
  const ps = paths.map((p) => ' --path ' + JSON.stringify(p)).join('')
  const schema = { type: 'object', properties: { files: { type: 'array', items: { type: 'object',
    properties: { path: { type: 'string' }, added: { type: 'integer' }, deleted: { type: 'integer' } },
    required: ['path', 'added', 'deleted'] } }, outside: ST_LIST }, required: ['files', 'outside'] }
  const r = await stageAsk(ctx, ST_MEASURER, 'numstat:' + label, 'Measure', ctx.cd +
    'Read-only: run `claude-code agents diff --base HEAD' + ps + ' --format json`; return its files and outside lists verbatim.', schema)
  if (!r) return null
  const m = {}, peers = new Set((r.outside || []).map((f) => stRel(ctx, f)))
  for (const x of r.files || []) if (!peers.has(stRel(ctx, x.path))) m[stRel(ctx, x.path)] = { a: x.added || 0, d: x.deleted || 0 }
  return m
}
// stDelta: what changed between two snapshots, split production/test per package (pkg: a dir or file -> dir); rows go to ST_LINES.
function stDelta(stage, pkg, before, after) {
  const zero = () => ({ add: 0, del: 0, net: 0 })
  const tot = { stage, pkg: typeof pkg === 'string' ? pkg : '*', measured: !!(before && after), prod: zero(), test: zero(), files: [] }
  if (!tot.measured) { ST_LINES.push(tot); return tot }
  const rows = {}
  for (const f of new Set([...Object.keys(before), ...Object.keys(after)])) {
    const b = before[f] || { a: 0, d: 0 }
    const x = after[f] || { a: 0, d: 0 }
    if (b.a === x.a && b.d === x.d) continue
    const p = typeof pkg === 'string' ? pkg : pkg(f)
    const row = (rows[p] = rows[p] || { stage, pkg: p, measured: true, prod: zero(), test: zero(), files: [] })
    for (const r of [row, tot]) {
      const k = ST_TEST_RE.test(f) ? r.test : r.prod
      k.add += x.a - b.a; k.del += x.d - b.d; k.net += x.a - b.a - (x.d - b.d); r.files.push(f)
    }
  }
  ST_LINES.push(...Object.values(rows))
  return tot
}
// stLines folds measured rows (this run's and its children's) into per-package and total production/test lines.
function stLines(rows) {
  const sum = (xs, k) => xs.reduce((n, r) => ({ add: n.add + r[k].add, del: n.del + r[k].del, net: n.net + r[k].net }), { add: 0, del: 0, net: 0 })
  const byPkg = {}
  for (const p of [...new Set(rows.filter((r) => r.measured).map((r) => r.pkg))].sort()) { const rs = rows.filter((r) => r.measured && r.pkg === p); byPkg[p] = { prod: sum(rs, 'prod'), test: sum(rs, 'test') } }
  const ok = rows.filter((r) => r.measured)
  return { stages: rows, byPkg, total: { prod: sum(ok, 'prod'), test: sum(ok, 'test') }, unmeasured: rows.filter((r) => !r.measured).map((r) => r.stage + ':' + r.pkg) }
}
// stTail: the report's measured sections: lines table, over-budget packages, proposed tests.
function stTail(lines, over, proposed) {
  const c = (x) => '+' + x.add + '/-' + x.del + ' (net ' + (x.net > 0 ? '+' : '') + x.net + ')'
  return ['', '## Lines (git diff --numstat, per package)', '| package | production | test |', '| --- | --- | --- |',
    ...Object.entries(lines.byPkg).map(([p, v]) => '| ' + p + ' | ' + c(v.prod) + ' | ' + c(v.test) + ' |'), '| **total** | ' + c(lines.total.prod) + ' | ' + c(lines.total.test) + ' |',
    ...(lines.unmeasured.length ? ['', 'Unmeasured (numstat reader returned nothing): ' + lines.unmeasured.join(', ')] : []),
    ...(over.length ? ['', '## Over line budget', ...over.map((o) => '- ' + o.pkg + ': production net ' + o.net + ' under ' + o.budget + (o.why ? ' — ' + o.why : ''))] : []),
    ...(proposed.length ? ['', '## Proposed tests (not written; the owner decides)', ...proposed.map((p) => '- ' + p.pkg + ': ' + p.case + ' — ' + p.reason + ' (' + p.runtime + ')')] : [])].join('\n')
}
// stGateRed: dirs whose LAST covering gate in ST_GATES (completion order) is not green; a dir no gate covers is red.
function stGateRed(dirs) {
  const cover = (g, d) => g.dir === '.' || g.dir === d || d.startsWith(g.dir + '/')
  return [...new Set(dirs)].filter((d) => { const g = [...ST_GATES].reverse().find((x) => cover(x, d)); return !g || g.exit !== 'green' }).sort()
}
// stArgs: the first top-level arg a script does not read, as an error message ('' when every key is known).
function stArgs(a, known) {
  const bad = Object.keys(a).filter((k) => !known.includes(k))
  return bad.length ? 'unknown arg ' + bad.map((k) => 'args.' + k).join(', ') + ' (known: ' + known.join('|') + ')' : ''
}
// stageAsk runs one stage agent; a throw becomes null (cause in ST_ERR); st.done records an external stage's ran-on.
const ST_ERR = {}
const stWhy = (label, msg) => (ST_ERR[label] ? msg + ': ' + ST_ERR[label] : msg)
const stageAsk = (ctx, st, label, ph, prompt, schema, pin) => agent(st.task(prompt), {
  label, phase: ph, schema: st.schema(schema), ...st.type,
  model: (ctx.model || ((m) => m))((!st.ext && pin && pin.model) || st.model), effort: (pin && pin.effort) || st.effort,
}).catch((e) => { ST_ERR[label] = String((e && e.message) || e); log(label + ' failed: ' + ST_ERR[label]); return null }).then((r) => st.done(label, r))
// stageDetect reads the scoped gate from `claude-code worktree gate`; no --lint-check: repo-wide lint trips on peers' WIP.
async function stageDetect(ctx, st, scope) {
  phase('Detect')
  const d = await stageAsk(ctx, st, 'detect', 'Detect', `${ctx.cd}Run exactly: claude-code worktree gate --dir ${ctx.cwd || '.'}${scope.length ? ' --scope ' + scope.join(',') : ''} --format json </dev/null — and return its gate and source fields verbatim (exit 1 or an error: gate "" and source "none"). Do NOT run the gate. Run nothing else.`, ST_DETECT)
  return { gate: (d && d.gate) || '', source: (d && d.source) || 'none' }
}
// stLoopCheck folds detection, routing pressure and the first observed check into one read-only leaf.
async function stLoopCheck(ctx, o, gate, label) {
  const paths = o.files.length ? o.files : o.scope
  const detect = 'claude-code worktree gate --dir ' + JSON.stringify(ctx.cwd || '.') +
    ' --scope ' + paths.join(',') + ' --format json'
  const cmd = Array.isArray(gate) ? gate.join(' && ') : gate
  return stageAsk(ctx, o.check || ctx.S.gate || ctx.S.detect, label, 'Gate', ctx.cd +
    'Read-only: edit nothing. ' + (cmd ? 'Gate command: ' + cmd : 'Detect the scoped gate with `' + detect + '` then use its gate.') +
    ' Run the gate exactly once with pipefail; capture the real process exit code. ' +
    'Run `git status --porcelain` scoped to ' + JSON.stringify(paths) + '; return changed as one string per listed path: ' +
    '"<path> <git hash-object path output, or deleted>" (the bare path when hash-object is refused). ' +
    'Return gate, exit (null only if execution is impossible), changed, failures [{file,line,text}] from command output. ' +
    'Keep failing paths repo-relative; retain errors without source locations too. ' +
    (ctx.claudeState ? 'Account state is ' + ctx.claudeState :
      'Also run `claude-code gateway --format json --agent claude --fallback none --model sonnet` and return candidates[0].state or unknown as state.'), ST_CHECK)
}
// stStamps maps each `changed` entry to its path; the entry (path + content hash) differs when the content does.
const stStamps = (changed) => new Map(changed.map((s) => [s.replace(/ (?:[0-9a-f]{40,64}|deleted)$/, ''), s]))
const stFailureKey = (f) => f.file + ':' + f.text.split('\n')[0].replace(/\b\d+(?:\.\d+)?(?:ms|s)\b/g, '').replace(/\/tmp\/[^ :]+/g, '<tmp>').trim()
const stOwned = (o, file) => !!file && !file.split('/').includes('..') && !file.startsWith('/') && (o.files.length ? o.files.includes(file) : o.scope.some((p) => p === '.' || file === p || file.startsWith(p + '/')))
function stLoopGroups(ctx, o, failures) {
  const groups = new Map()
  for (const f of failures) {
    const file = stRel(ctx, f.file)
    if (!stOwned(o, file)) continue
    const key = file.endsWith('.go') ? stDir(file) : file
    if (!groups.has(key)) groups.set(key, [])
    groups.get(key).push({ ...f, file })
  }
  return [...groups].sort((a, b) => b[1].length - a[1].length).slice(0, o.fixes || o.fixesPerIteration || 3)
}
async function stLoopFix(ctx, o, groups, iteration) {
  const run = async ([file, fs]) => {
    const dir = fs[0].file !== file
    const prompt = ctx.cd + ctx.rules + ' Fix only this owned ' + (dir ? 'package directory (its files, not subdirectories; a failing test line may have its cause in the code under test): ' : 'file: ') + file +
      '. Never run the gate, tests, or self-checks; the read-only check owns verification. Other failures are not yours.\n' +
      fs.map((f) => f.file + ':' + (f.line || 0) + ': ' + f.text).join('\n').slice(0, 1500)
    const label = (o.label || 'gate') + ':fix:' + iteration + ':' + file
    return await stageAsk(ctx, o.fix || ctx.S.fix, label, 'Fix', prompt, ST_WORK) ||
      await stageAsk(ctx, o.fix || ctx.S.fix, label + ':retry', 'Fix', prompt, ST_WORK)
  }
  const results = []
  for (let i = 0; i < groups.length; i += o.parallel || 4)
    results.push(...await parallel(groups.slice(i, i + (o.parallel || 4)).map((g) => () => run(g))))
  return results.every(Boolean)
}
const stGateCmd = (o) => o.gate || (o.gates ? Object.entries(o.gates).filter(([ext]) => !o.files.length || o.files.some((f) => f.endsWith('.' + ext))).map(([, cmd]) => cmd).join(' && ') : '')
// stRunnable: the check read an exit code from a gate that ran (126 and 127 mean it could not).
const stRunnable = (c) => !!c && Number.isInteger(c.exit) && ![126, 127].includes(c.exit)
const stCheckOk = (r) => !!r && Array.isArray(r.changed) && Array.isArray(r.failures) && r.failures.every((f) => !!f && typeof f.file === 'string' && typeof f.text === 'string')
const stMoved = (prev, now, base) => base ? [] : [...new Set([...now.keys(), ...(prev || new Map()).keys()])].filter((k) => !prev || prev.get(k) !== now.get(k))
const stFirstExit = (o, c) => !stRunnable(c) ? 'error' : c.exit === 0 ? 'green' : o.max === 0 ? 'red' : 'capped'
const stNextExit = (c, fixed, removed) => !stRunnable(c) ? 'error' : c.exit === 0 ? 'green' : !fixed ? 'error' : !c.changed.length || !removed ? 'stalled' : 'capped'
// stWithState: adopt the first check's account state; the model mapping follows the latest check (L.cur).
const stWithState = (ctx, L) => L.cur && L.cur.state && !ctx.claudeState ? { ...ctx, claudeState: L.cur.state, model: (m) => ['low', 'exhausted'].includes(L.cur.state) ? ({ opus: 'sonnet', sonnet: 'haiku' }[m] || m) : m } : ctx
const ST_NO_SCOPE = 'gate requires scope or files; check unavailable'
async function stLoopObserve(ctx, o, L, base) {
  const r = await stLoopCheck(ctx, o, L.gate, (o.label || 'gate') + ':check:' + L.gateRuns)
  L.gateRuns++
  if (!stCheckOk(r)) return null
  L.gate = L.gate || r.gate
  const now = stStamps(r.changed), moved = stMoved(L.stamps, now, base)
  L.stamps = now
  for (const f of moved) L.changed.add(f)
  return { ...r, changed: moved }
}
async function stLoopRound(ctx, o, L, before, i) {
  const groups = stLoopGroups(ctx, o, before.failures)
  const fixed = await stLoopFix(ctx, o, groups, i)
  const cur = await stLoopObserve(ctx, o, L)
  const removed = cur && before.failures.some((f) => !cur.failures.some((g) => stFailureKey(f) === stFailureKey(g)))
  const entry = { iteration: i, exitBefore: before.exit, exitAfter: cur ? cur.exit : null, failuresBefore: before.failures.length,
    failuresAfter: cur ? cur.failures.length : 0, changed: cur ? cur.changed : [], groups: groups.map(([f]) => f) }
  return { cur, entry, exit: stNextExit(cur, fixed, removed) }
}
const stLoopOpts = (o) => ({ ...o, scope: o.scope || [], files: o.files || [] })
const stLoopStart = (ctx, o, L, cap) => o.trust && o.known && o.known.length && cap ? { exit: 1, changed: [], failures: o.known } : stLoopObserve(ctx, o, L, true)
// stageLoop preserves {scope, gate, max, check, fix}; only observed checks determine exit and residual.
async function stageLoop(ctx, o) {
  o = stLoopOpts(o)
  const used = o.iterationsUsed || 0
  const cap = Math.max(0, Math.min(o.max === undefined ? 3 : o.max, 5 - used))
  const L = { gate: stGateCmd(o), gateRuns: 0, stamps: null, changed: new Set(), cur: null }
  const history = []
  let exit = 'error'
  if (o.scope.length || o.files.length) {
    L.cur = await stLoopStart(ctx, o, L, cap)
    ctx = stWithState(ctx, L)
    exit = stFirstExit(o, L.cur)
    for (let i = 1; exit === 'capped' && i <= cap; i++) {
      const r = await stLoopRound(ctx, o, L, L.cur, i)
      L.cur = r.cur
      history.push(r.entry)
      exit = r.exit
    }
  }
  const residual = L.cur ? L.cur.failures : [{ file: '', line: 0, text: ST_NO_SCOPE }]
  ST_GATES.push(...(o.scope.length ? o.scope : o.files.map(stDir)).map((dir) => ({ dir, exit, label: o.label || 'gate' })))
  return { exit, gate: L.gate, iterations: history.length, iterationsUsed: used + history.length,
    filesChanged: [...L.changed], residual, gateRuns: L.gateRuns, exitCode: L.cur ? L.cur.exit : null, history }
}
const ST_MERGE = ' These are consolidation findings (duplicates, dead code, legacy, stale or redundant tests), not bugs: merge and delete; behaviour, messages and exit codes stay identical.'
const ST_FIX_HOW = ' The evidence is verified: do not re-audit; first tool call: cat the cited files. The Test stage repairs tests; that is not unfinished work, so report done. ' +
    'Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 4-6). Root cause first, smallest correct change, functions < 50 lines, 1-line comments. ' +
    'Grep every caller outside the package of each function you changed and report in `callers` whether any now duplicates or contradicts the new behavior (do not edit them). ' +
  'Run the repo formatter and linter on what you touched'
const stList = (x, k) => (x && x[k]) || []
const stFixItem = (ctx, f) => '- ' + stRel(ctx, f.file) + ':' + (f.line || 0) + ' (' + (f.symbol || '?') + ', ' + f.severity + (stCost(f) ? ', ' + f.kind : '') + '): ' + (f.symptom || f.title) + (f.scenario ? '\n  scenario: ' + f.scenario : '') + (f.evidence ? '\n  evidence: ' + f.evidence : '')
const stFixOwn = (o) => o.owns && o.owns.length ? ' You own ONLY: ' + o.owns.join(', ') + '; list any other file you would need in leftUndone.' : ' Edit only non-test files under ' + o.pkg + ', except a *_test.go file a finding below cites.'
const stFixBrief = (ctx, o, own, list) => {
  const merge = ctx.kind === 'consolidate' ? ST_MERGE : ''
  const seam = ctx.seams ? ' Never change an exported signature or edit outside ' + o.pkg + ': when a fix needs either, leave that part undone and list it in `seams` as {file, need}.' : ''
  return ctx.rules + ' Fix these confirmed ' + (merge ? 'findings' : 'defects') + ' in ' + o.pkg + ' and nothing else.' + own + merge + seam + ST_FIX_HOW + (o.owns ? '' : ' (gofmt + go vet ./' + o.pkg + '/... for Go)') + '. Return `files`: every repo-relative file you changed.\n' + list
}
const stTestBrief = (ctx, o, impl, tdirs, list) => ctx.rules + ' Fixes just landed in ' + o.pkg + ' (' + stDiff([o.pkg]) + '; changed: ' + (impl.files || []).join(', ') + '). ' +
    'Run the tests of ' + tdirs.join(', ') + ' and repair only the tests these fixes broke. Edit only *_test.go and testdata files directly in ' + tdirs.join(', ') + '; never another package\'s tests or shared fixtures (list any you would need in leftUndone). ' +
    'Write NO new test.' + ST_PROPOSE + ' Put "n/a" in callers.\nThe fixes:\n' + list
const stFixDone = (impl) => !!(impl && (impl.status === 'done' || (impl.status === 'partial' && impl.seams && impl.seams.length && !(impl.leftUndone || []).length)))
const stTdirs = (ctx, o, impl) => [...new Set([o.pkg, ...stList(impl, 'files').map((f) => stDir(stRel(ctx, f))).filter((d) => d === o.pkg || d.startsWith(o.pkg + '/'))])]
const stFixFiles = (ctx, impl, b, test) => [...new Set([...stList(impl, 'files'), ...stList(b.shrink, 'files'), ...stList(test, 'files')].map((f) => stRel(ctx, f)))]
// stageFix: one implementer over confirmed findings in one package, measured and held to ctx.lineBudget, then the Test stage.
async function stageFix(ctx, o) {
  const list = o.findings.map((f) => stFixItem(ctx, f)).join('\n')
  const own = stFixOwn(o)
  const at = o.owns && o.owns.length ? o.owns : [o.pkg]
  const pre = await stSnap(ctx, 'pre:' + o.label, at)
  const impl = await stageAsk(ctx, ctx.S.fix, 'fix:' + o.label, 'Fix', stFixBrief(ctx, o, own, list), ST_FIXW, o.pin)
  const done = stFixDone(impl)
  for (const s of stList(impl, 'seams')) ST_SEAMS.push({ pkg: o.pkg, file: stRel(ctx, s.file), need: s.need })
  const b = await stBudget(ctx, o, { task: own + '\n' + list, done, pre, at })
  // the owner's test rule: repair what the edits broke, write nothing new, propose the rest with a reason and a runtime
  const test = done && o.tests !== false ? await stageAsk(ctx, ctx.S.test, 'test:' + o.label, 'Test', stTestBrief(ctx, o, impl, stTdirs(ctx, o, impl), list), ST_TESTW) : null
  for (const p of stList(test, 'proposed')) ST_PROPOSED.push({ pkg: o.pkg, ...p })
  let snap = b.snap
  if (test) { const s = await stSnap(ctx, 'test:' + o.label, at); stDelta('test', o.pkg, snap, s); snap = s }
  const files = stFixFiles(ctx, impl, b, test)
  return { pkg: o.pkg, impl, test, done, testOk: o.tests === false || !!(test && test.status === 'done'), files, landed: b.files, over: b.over, snap }
}
const stChanged = (pre, snap) => Object.keys(snap || {}).filter((f) => pre && JSON.stringify(pre[f]) !== JSON.stringify(snap[f]))
async function stShrink(ctx, o, w, net, snap) {
  const shrink = await stageAsk(ctx, ctx.S.fix, 'shrink:' + o.label, 'Fix', ctx.rules + ' Line budget ' + ctx.lineBudget + ': production net must be ≤ ' + ST_BUDGET[ctx.lineBudget] + ' lines in ' + o.pkg +
      '; your fix measured ' + net + ' (git diff --numstat). Shrink it or justify it: cut what the fix does not need (reuse an existing helper, drop a new abstraction, delete what it made dead) and keep the fix.' +
      ' Never revert the fix, never git checkout/restore/stash. If it cannot shrink, edit nothing and put the reason in summary.' + w.task, ST_WORK)
  const s = await stSnap(ctx, 'shrink:' + o.label, w.at)
  return { shrink, snap: s, net: net + stDelta('shrink', o.pkg, snap, s).prod.net }
}
// stBudget measures the fix; over ctx.lineBudget it gets ONE shrink-or-justify iteration, still over lands in ST_OVER (never reverted).
async function stBudget(ctx, o, w) {
  let snap = await stSnap(ctx, 'fix:' + o.label, w.at)
  const fr = stDelta('fix', o.pkg, w.pre, snap)
  const live = w.done && fr.measured
  let net = fr.prod.net, shrink = null
  if (live && stOver(ctx, net)) ({ shrink, snap, net } = await stShrink(ctx, o, w, net, snap))
  const over = live && stOver(ctx, net) ? { pkg: o.pkg, net, budget: ctx.lineBudget, why: (shrink && shrink.summary) || '' } : null
  if (over) ST_OVER.push(over)
  return { snap, shrink, over, files: [...new Set([...fr.files, ...stChanged(w.pre, snap)])] }
}
// stageRepair: stageFix, then stageLoop (ctx.gate, else a Go gate) once the fix is done; green is the gate's, never a stage's claim.
async function stageRepair(ctx, pkg, findings, o) {
  const fx = await stageFix(ctx, { pkg, findings, label: o.label })
  const gate = fx.landed.length ? await stageLoop(ctx, { scope: [o.scope || pkg], check: ctx.S.gate, fix: ctx.S.fix, max: fx.done ? ctx.gateMax || 0 : 0, label: o.gate, gate: ctx.gate }) : null
  return { ...fx, gate, green: fx.done && !!gate && gate.exit === 'green', files: [...fx.files, ...(gate ? gate.filesChanged : [])] }
}
function stPolishNone(preRed) {
  const d = preRed.map((p) => p + ': red-before-polish')
  return { dedupe: null, simplify: null, review: { findings: [], ran: false }, fixall: [], incomplete: [], green: !preRed.length, red: preRed, hold: [], files: [],
    decisions: d, report: d.join('\n') }
}
const stCutNote = (pkgs, preRed) => ' Name the assumption before every cut; flag instead of cutting when it has no covering test; never cut semantic comments, error context or distinct tests; never fix bugs (flag them). ' +
    'After edits run gofmt, go vet and go test for ' + pkgs.filter((p) => !preRed.includes(p)).join(', ') + '; if a test regresses, revert your own edit by hand (never git checkout/restore/stash) and flag it. ' +
    'Your net lines (measured by git diff --numstat afterwards) must be ≤ 0: remove more than you add, or apply nothing. ' +
    'Return applied[] and flagged[] as `file:line · what · assumption`, deltaLines (net changed lines, negative = removed) and status (clean = nothing to do).'
async function stHeld(ctx, P, k, r) {
  const s = await stSnap(ctx, k, P.files)
  const d = stDelta(k, stDir, P.snap, s)
  P.snap = s
  const net = d.prod.net + d.test.net
  const out = r && d.measured && net > 0 ? { ...r, status: 'partial', measured: net } : r ? { ...r, measured: d.measured ? net : null } : r
  P.lines.push(k + ': ' + (out ? out.applied.length + ' applied, ' + out.flagged.length + ' flagged, measured Δ ' + (d.measured ? net : '?') + ' lines (' + out.status + (net > 0 ? ': net must be ≤ 0' : '') + ')' : 'agent returned nothing'))
  return out
}
const stDedupeBrief = (ctx, P) => ctx.rules + ' Never edit packages red before polish: ' + P.preRed.join(', ') + '. Cross-package dedupe, the reuse angle of ' + ctx.cwd + '/skills/simplify/SKILL.md.' + P.scope +
      ' Look ONLY for helpers or logic duplicated across these packages (grep siblings, `git log -S`); compare a near-duplicate with its original for behavior the copy lost, then consolidate at the original (callers import it; no new shim).' + P.cut
const stSimplifyBrief = (ctx, P) => ctx.rules + ' Never edit packages red before polish: ' + P.preRed.join(', ') + '. Simplify gate (' + ctx.cwd + '/skills/simplify/SKILL.md, quality.md -> Simplify), run ONCE over the merged diff.' + P.scope +
    ' Run `claude-code review simplify ' + P.files.join(' ') + '`, then one in-context pass (reuse, simplification, efficiency, altitude). Edit only the listed non-test files.' + P.cut
async function stPolishPass(ctx, P, key, title, brief) {
  phase(title)
  return stHeld(ctx, P, key, await stageAsk(ctx, ctx.S[key], key, title, brief, ST_SIMP))
}
async function stReview(ctx, P) {
  phase('Review')
  const rv = await stageAsk(ctx, ctx.S.review, 'review', 'Review', ctx.rules + ' Read-only: edit nothing, never run go test. Review the final diff with the ' + ctx.cwd +
    '/skills/evaluate-code/SKILL.md correctness and contract lenses (wrong results, broken invariants, changed behavior callers rely on, errors dropped).' + P.scope +
    ' Each finding: file, line, symbol, one-line symptom, severity. Return {findings: []} when clean.', ST_REVF)
  const findings = rv ? rv.findings.map((f) => ({ ...f, file: stRel(ctx, f.file) })) : []
  P.lines.push('review: ' + (rv ? findings.length + ' finding(s)' : 'agent returned nothing'))
  return { rv, findings }
}
// a finding outside the diff's packages has no Fix-all owner: unresolved, and MEDIUM+ keeps the polish red
function stSplit(findings, pkgs) {
  const byPkg = {}
  for (const f of findings) if (pkgs.includes(stDir(f.file))) (byPkg[stDir(f.file)] = byPkg[stDir(f.file)] || []).push(f)
  const incomplete = findings.filter((f) => !pkgs.includes(stDir(f.file)))
  return { byPkg, incomplete, open: incomplete.filter((f) => f.severity !== 'low') }
}
// a fixer edits under its package, nested ones too: run in waves by ancestor count, so ancestors finish first
async function stFixAll(ctx, byPkg, keys) {
  const depth = (p) => keys.filter((q) => q !== p && (q === '.' || p.startsWith(q + '/'))).length
  const byKey = {}
  for (const w of [...new Set(keys.map(depth))].sort((x, y) => x - y)) {
    const ps = keys.filter((p) => depth(p) === w)
    const rs = await parallel(ps.map((p) => async () => {
      const r = await stageRepair(ctx, p, byPkg[p], { label: 'fixall:' + p, gate: 'regate:' + p })
      return { pkg: p, findings: byPkg[p].length, done: r.done, green: r.green, gate: r.gate ? r.gate.exit : 'none', files: r.files }
    }))
    ps.forEach((p, i) => { byKey[p] = rs[i] })
  }
  return keys.map((p) => byKey[p]).filter(Boolean)
}
// a Dedupe/Simplify edit no Fix-all re-gated gets a gate of its own (unmeasured edits: every package); its green is never assumed
async function stPolishGates(ctx, P, passes, keys) {
  const unknown = passes.some((r) => r && r.measured === null && r.applied.length)
  const edited = (unknown ? P.pkgs : [...new Set(ST_LINES.filter((r) => r.measured && /^(dedupe|simplify)$/.test(r.stage)).map((r) => r.pkg))]).filter((p) => !keys.includes(p))
  return parallel(edited.filter((p) => !P.preRed.includes(p)).map((p) => async () => {
    const r = await stageLoop(ctx, { scope: [p], files: P.files.filter((f) => stDir(f) === p), check: ctx.S.gate,
      fix: ctx.S.fix, max: ctx.gateMax || 0, label: 'polish-gate:' + p, gate: ctx.gate })
    P.lines.push('polish gate ' + p + ': ' + r.exit)
    return { pkg: p, ...r }
  }))
}
function stPolishEnd(P, x) {
  const report = ['', '## Polish', ...P.lines.map((l) => '- ' + l), ...x.findings.map((f) => '- **' + f.severity + '** `' + f.file + ':' + f.line + '` (' + f.symbol + ') — ' + f.symptom)].join('\n')
  // a review that never ran proves nothing, and a MEDIUM+ finding with no owner stays open: both hold red whatever a gate says
  const hold = [...new Set([...x.open.map((f) => stDir(f.file)), ...(x.rv ? [] : P.pkgs)])]
  const red = [...new Set([...x.fixall.filter((g) => !g.green).map((g) => g.pkg), ...x.pg.filter((g) => g.exit !== 'green').map((g) => g.pkg), ...P.preRed, ...hold])]
  return { dedupe: x.dedupe, simplify: x.simplify, review: { findings: x.findings, ran: !!x.rv }, fixall: x.fixall, incomplete: x.incomplete, green: !red.length, red, hold,
    files: [...new Set([...x.fixall.flatMap((g) => g.files), ...x.pg.flatMap((g) => g.filesChanged)])], decisions: P.lines, report }
}
// stagePolish: once over a merged diff: Dedupe (2+ packages), Simplify, Review, then one stageRepair per package.
async function stagePolish(ctx, o) {
  const allFiles = [...new Set(o.files.map((f) => stRel(ctx, f)))]
  const pkgs = [...new Set(allFiles.map(stDir))]
  const preRed = stGateRed(pkgs.filter((p) => ST_GATES.some((g) => g.dir === p || g.dir === '.')))
  const files = allFiles.filter((f) => !preRed.includes(stDir(f)))
  if (!files.length) return stPolishNone(preRed)
  const scope = ' Only these files (the tree is shared; never touch or diff others): ' + files.join(' ') + '. Diff: ' + stDiff(files) + '.'
  // Dedupe and Simplify are held to a measured net ≤ 0 whatever the budget; over it they report partial with the number
  const P = { files, pkgs, preRed, scope, cut: stCutNote(pkgs, preRed), lines: [], snap: await stSnap(ctx, 'polish', files) }
  let dedupe = null
  if (pkgs.length >= 2 && ctx.S.dedupe) dedupe = await stPolishPass(ctx, P, 'dedupe', 'Dedupe', stDedupeBrief(ctx, P))
  else P.lines.push('dedupe skipped: ' + pkgs.length + ' package(s)')
  const simplify = await stPolishPass(ctx, P, 'simplify', 'Simplify', stSimplifyBrief(ctx, P))
  const { rv, findings } = await stReview(ctx, P)
  const { byPkg, incomplete, open } = stSplit(findings, pkgs)
  if (incomplete.length) P.lines.push('unresolved: ' + incomplete.length + ' review finding(s) outside the fixed packages (' + open.length + ' MEDIUM+): ' + incomplete.map((f) => f.file + ':' + f.line).join(', '))
  const keys = Object.keys(byPkg).filter((p) => !preRed.includes(p))
  for (const p of preRed) P.lines.push(p + ': red-before-polish')
  const fixall = await stFixAll(ctx, byPkg, keys)
  for (const x of fixall) P.lines.push('fix-all ' + x.pkg + ': ' + (x.done ? x.findings + ' fixed' : 'NOT done') + ', gate ' + (x.green ? 'green' : 'red'))
  const pg = await stPolishGates(ctx, P, [dedupe, simplify], keys)
  return stPolishEnd(P, { dedupe, simplify, rv, findings, fixall, incomplete, open, pg })
}
// ── end stages ──
const ARGS = ['cwd', 'scope', 'files', 'gate', 'gates', 'known', 'trust', 'maxIterations', 'iterationsUsed',
  'fixesPerIteration', 'parallel', 'measure', 'claudeState', 'rules', 'steps', 'profile', 'provider']
const fail = (text) => ({ exit: 'error', gate: a.gate || '', gateSource: 'none', iterations: 0,
  iterationsUsed: a.iterationsUsed || 0, filesChanged: [], residual: [{ file: '', line: 0, text }],
  gateRuns: 0, exitCode: null, history: [], gates: [], coworkers: COWORKERS })
const strList = (v) => Array.isArray(v) && v.every((p) => typeof p === 'string' && p.trim())
const okGate = (g) => (typeof g === 'string' && !!g.trim()) || (Array.isArray(g) && g.length > 0 && g.every((c) => typeof c === 'string' && c.trim()))
const okGates = (g) => !!g && !Array.isArray(g) && typeof g === 'object' && Object.keys(g).length > 0 && Object.values(g).every((c) => typeof c === 'string' && c.trim())
const okKnown = (k) => Array.isArray(k) && k.every((f) => f && typeof f.file === 'string' && typeof f.text === 'string')
// perKey: a check that names the first of ks whose given value is bad.
const perKey = (ks, bad, msg) => (x) => { const k = ks.find((k) => x[k] !== undefined && bad(x[k], k)); return k && k + msg }
// The args checks in order: each returns the error message, or a falsy value when the args pass it.
const LOOP_CHECKS = [
  (x) => stArgs(x, ARGS),
  perKey(['scope', 'files'], (v) => !strList(v), ' must be a path array'),
  (x) => !(x.scope || []).length && !(x.files || []).length && 'gate requires scope or files',
  (x) => x.gate && x.gates && 'gate and gates are mutually exclusive',
  (x) => x.gate !== undefined && !okGate(x.gate) && 'gate must be a command or command array',
  (x) => x.gates !== undefined && !okGates(x.gates) && 'gates must map languages to commands',
  (x) => x.known !== undefined && !okKnown(x.known) && 'known must contain file/text failures',
  perKey(['trust', 'measure'], (v) => typeof v !== 'boolean', ' must be boolean'),
  perKey(['maxIterations', 'iterationsUsed', 'fixesPerIteration', 'parallel'],
    (v, k) => !Number.isInteger(v) || v < (['parallel', 'fixesPerIteration'].includes(k) ? 1 : 0), ' must be a valid integer cap'),
  perKey(['profile', 'provider'], (v) => !/^[A-Za-z0-9._-]+$/.test(v), ' must be a plain name'),
]
async function looperRun() {
  const opt = { allowed: ['detect', 'fix'], external: true, profile: a.profile, provider: a.provider }
  const S = { detect: stepOf(a.steps, 'detect', { model: 'haiku', effort: 'low', tools: 'run' }, opt),
    fix: stepOf(a.steps, 'fix', { model: 'sonnet', effort: 'medium', tools: 'edit' }, opt) }
  const ctx = { cwd: a.cwd || '', cd: a.cwd ? 'Run every command in cwd ' + JSON.stringify(a.cwd) + '. ' : '',
    rules: a.rules || 'Never git stash|checkout|reset|restore|clean|commit; never install/deploy/release or edit tool config.',
    S, claudeState: a.claudeState }
  const at = a.files && a.files.length ? a.files : a.scope
  const pre = a.measure ? await stSnap(ctx, 'loop:before', at) : null
  const g = await stageLoop(ctx, { ...a, max: a.maxIterations === undefined ? 3 : a.maxIterations, check: S.detect, fix: S.fix })
  if (a.measure) stDelta('gate', stDir, pre, await stSnap(ctx, 'loop:after', at))
  return { ...g, gateSource: a.gate || a.gates ? 'args' : g.gate ? 'detected' : 'none',
    ...(a.measure ? { lines: stLines(ST_LINES) } : {}), gates: ST_GATES, coworkers: COWORKERS }
}
async function runLooper() {
  try {
    for (const check of LOOP_CHECKS) { const m = check(a); if (m) throw new Error(m) }
    return await looperRun()
  } catch (e) { return fail('code-looper: ' + e.message) }
}
return runLooper()
