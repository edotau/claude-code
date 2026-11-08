export const meta = {
  name: 'code-analyzer',
  description: 'Audit a change set: parallel scoped auditors → refute each finding → per-package implementer, then test agent → gate loop',
  whenToUse:
    'USE: audit before a merge or after a refactor; review-only with fix:false. CALL: Workflow({scriptPath: "~/.claude/workflows/code-analyzer.js", args: {cwd, paths: [...] | range: "main..HEAD" | findings}}). OPTIONAL: fix (default true), partition (with fix:false: return the wave plan, no edits), cap (wave width, default 8), hotspots (extra single-owner files), minFix, rounds, dimensions (replaces the lenses), tests, gate (command override), maxIterations (gate loop cap), prior (earlier findings to skip), chunkChars (chunk size), refuteBatch (findings per skeptic), steps {scope, audit, refute, fix, test, gate, depscan} = {agent?, model?, effort?, profile?, provider?} per stage (agent claude|codex|copilot|gemini|opencode; external stages run on the session\'s route, or args/steps profile|provider, and fall back on 429/auth; each reports ran on or BLOCKED). OPTIONAL: profile, provider (run-wide override for external stages). COST: about 8 agent slots per run, so one run at a time. RETURNS {exit, route, packages, merged, confirmed, dropped, unverified, fixed, red, decisions, report, partition} plus refuted, rounds. Details: saved-workflows.md.',
  phases: [
    { title: 'Scope', detail: 'one cheap agent per package group returns per-file sizes; the script packs them into ≤50 KB chunks' },
    { title: 'Find', detail: 'one auditor per (dimension × chunk) reading its chunk in-context — agent count scales with diff size' },
    { title: 'Refute', detail: 'one skeptic per package batch (≤6 findings); re-anchors wrong citations, defaults to refuted when the evidence is thin' },
    { title: 'Scan', detail: 'one read-only agent per package with fixable findings: files, exported, importers, hotspots' },
    { title: 'Plan', detail: 'script-only conflict graph, greedy colouring into waves ≤cap; hotspot edits go to one final task' },
    { title: 'Fix', detail: 'waves in sequence, tasks of a wave in parallel; per package: implementer (opus only for highs), then test agent with its result' },
    { title: 'Gate', detail: 'green-keeper-loop workflow over the touched packages, then re-audits what it touched for up to rounds rounds' },
  ],
}

// ── Args ────────────────────────────────────────────────────────────────────
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const ERR = m => ({ exit: 'error', error: 'code-analyzer: ' + m, route: 'code-analyzer', decisions: [], report: '', unverified: [], rounds: [] })
const CWD = a.cwd || ''
const IN = CWD ? ' in ' + CWD : ''
const CD = CWD ? 'cd ' + CWD + ' && ' : ''
let PATHS = Array.isArray(a.paths) ? a.paths.filter(Boolean) : []
let PKGS = Array.isArray(a.packages) && a.packages.length ? a.packages : PATHS
// A pre-verified findings list (same shape as FINDINGS_SCHEMA items) skips Scope/Find/Refute and goes straight to Fix.
// Accept code-workers-shaped findings ({symbol, symptom, rederived}) as well as this script's {title, scenario, evidence}.
const asFinding = f => ({ ...f, title: f.title || f.symptom || f.symbol || '(untitled)', scenario: f.scenario || f.rederived || f.symptom || 'not given',
  evidence: f.evidence || (f.symbol ? `${f.symbol} at ${f.file}:${f.line} (caller-supplied)` : 'caller-supplied, not re-verified') })
let PRE = Array.isArray(a.findings) && a.findings.length ? a.findings.map(asFinding) : null
// No range, paths or findings → audit the uncommitted working tree (git diff HEAD).
const RANGE = a.range || (PATHS.length || PRE ? '' : 'HEAD')
const FIX = a.fix !== false
// partition:true with fix:false returns the wave plan (Scan + Plan) without editing anything.
const PART = a.partition === true
// Hotspot files are edited by one final single-owner task, not in the waves; '**/D/**' matches a directory.
const HOTSPOTS = ['internal/schemas/ledger_test.go', 'internal/network/web/assets/architecture.json', 'skills/subagent-workflows/references/saved-workflows.md', 'settings.json', 'go.mod', 'go.sum', 'Makefile', '**/testdata/**']
const HOT = [...HOTSPOTS, ...(Array.isArray(a.hotspots) ? a.hotspots.filter(Boolean) : [])]
// Wave width: args.cap, bounded by the dispatch hook's 24 per account.
const CAP = Math.min(Number.isInteger(a.cap) && a.cap > 0 ? a.cap : 8, 24)
// tests:false skips the per-package test agent for every package (no test agent).
if (a.tests !== undefined && typeof a.tests !== 'boolean') return ERR('args.tests must be a boolean')
const TESTS = a.tests !== false
const MAX_ITER = Number.isInteger(a.maxIterations) && a.maxIterations > 0 ? a.maxIterations : 3
// Rounds re-audit exactly the non-test source the previous round's fix touched, until dry/stalled/capped (D2: in-script loop, not self-recursion).
const ROUNDS = Number.isInteger(a.rounds) && a.rounds > 0 ? a.rounds : 2
// Prior findings as {file, line, symbol}; a string 'file:line' is accepted. Matched by proximity (same), not wording.
let PRIOR = (Array.isArray(a.prior) ? a.prior : []).map(p => typeof p === 'string' ? { file: p.replace(/:\d+$/, ''), line: +(p.match(/:(\d+)$/) || [0, 0])[1], symbol: '' } : p)
// Clean break: the per-stage args moved into steps.
const REPLACED = { auditModel: 'audit', refuteModel: 'refute', refuters: 'refute', profiles: 'refute' }
for (const k of Object.keys(REPLACED)) {
  if (a[k] !== undefined) return ERR(`${k} was replaced by steps.${REPLACED[k]} (plans/2026-11-03-workflow-steps.md)`)
}
const TEST_GLOBS = '*_test.go, testdata/, test_*.py, *_test.py, tests/, conftest.py, *.test.*, *.spec.*, __tests__/, fixtures/'
const SRC_RE = /\.(go|py|ts|tsx|js|jsx|mjs|rs|java)$/
const TEST_RE = /(_test\.(go|py)$|\.(test|spec)\.[a-z]+$|(^|\/)test_[^/]*\.py$|(^|\/)(tests?|__tests__|testdata|fixtures)\/)/
// Severity floor for the Fix phase: lows are reported, not fixed (round 1 spent 3 agents on one low). Set minFix:'low' to fix all.
const ORDER = { critical: 0, high: 1, medium: 2, low: 3 }
const MIN_FIX = ORDER[a.minFix] !== undefined ? ORDER[a.minFix] : ORDER.low
let TARGET = RANGE ? 'the diff of ' + RANGE : 'the code under ' + PATHS.join(', ')
const SHARED_TREE =
  'Shared live tree: never git stash|checkout|reset|restore|clean|commit, never make build|install|test|audit, never edit settings.json. ' +
  (CWD ? 'Prefix every shell command with `cd ' + CWD + ' &&`. ' : '') + 'Comments 1 line ≤120 chars.'

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
      ' The external agent does the whole task; never do it yourself or grade/answer in-context after a probe call. Copy the envelope\'s ran_on verbatim into ran_on, the length of its hops array in hops, and set coworker "ok". If every hop failed' +
      ' (429, auth, no route), set coworker "BLOCKED", coworker_error to the exact error, and the other fields to their empty values.\n\nTASK:\n' + p,
    schema: (s) => s ? { ...s, required: [...(s.required || []), ...R], properties: { ...s.properties, ...F } }
      : { type: 'object', additionalProperties: false, required: ['answer', ...R], properties: { answer: { type: 'string' }, ...F } },
    done: (label, r) => (record(label, r) ? r : null),
    text: (label, r) => (record(label, r) ? r.answer : null),
  }
}
// ── end stepOf ──

// ── Steps: who runs each stage; defaults reproduce the pre-steps models and efforts ──
const STAGES = ['scope', 'audit', 'refute', 'fix', 'test', 'gate']
const OPT = { allowed: [...STAGES, 'depscan'], external: true, profile: a.profile, provider: a.provider }
const RAW = a.steps && typeof a.steps === 'object' && !Array.isArray(a.steps) ? a.steps : {}
let S
try {
  S = {
    scope: stepOf(a.steps, 'scope', { model: 'haiku', effort: 'low' }, OPT),
    audit: stepOf(a.steps, 'audit', { model: 'sonnet', effort: 'low' }, OPT),
    refute: stepOf(a.steps, 'refute', { model: 'sonnet', effort: 'medium' }, OPT),
    fix: stepOf(a.steps, 'fix', { model: 'sonnet', effort: 'medium', tools: 'edit', agentType: 'claude' }, OPT),
    test: stepOf(a.steps, 'test', { model: 'sonnet', effort: 'medium', tools: 'edit', agentType: 'claude' }, OPT),
    gate: stepOf(a.steps, 'gate', {}, OPT),
    depscan: stepOf(a.steps, 'depscan', { model: 'sonnet', effort: 'medium' }, OPT),
  }
} catch (e) { return ERR(e.message) }
for (const k of ['profile', 'provider']) if (a[k] !== undefined && !/^[A-Za-z0-9._-]+$/.test(String(a[k]))) return ERR('args.' + k + ' must be a plain name')
const EXT = [...new Map(Object.values(S).filter((s) => s.ext).map((s) => [s.agent + '|' + s.profile + '|' + s.provider, s])).values()]
if (EXT.length) {
  const pf = await agent(
    'For each line, run it and report its candidate count and the first candidate as agent/account/model. Edit nothing.\n' +
      EXT.map((s) => `claude-code gateway router --agent ${s.agent} --fallback agents --profile ${s.profile}${s.provider ? ' --provider ' + s.provider : ''} --format json </dev/null`).join('\n'),
    { label: 'coworkers-preflight', phase: 'Preflight', model: 'haiku', effort: 'low', schema: { type: 'object', additionalProperties: false, required: ['routes'], properties: { routes: { type: 'array', items: { type: 'object', additionalProperties: false, required: ['agent', 'candidates', 'first'], properties: { agent: { type: 'string' }, candidates: { type: 'integer' }, first: { type: 'string' }, error: { type: 'string' } } } } } } },
  )
  for (const r of (pf && pf.routes) || []) log(`coworkers preflight ${r.agent}: ${r.candidates} route(s), first ${r.first}${r.error ? ' — ' + r.error : ''}`)
  const dead = ((pf && pf.routes) || []).filter((r) => r.candidates === 0).map((r) => r.agent + (r.error ? ' (' + r.error + ')' : ''))
  if (dead.length) return ERR('coworkers: no route for ' + dead.join(', ') + '; BLOCKED before any work')
}
// Audit effort stays per lens unless steps.audit.effort is set; fix keeps opus+high for highs unless set.
const AUDIT_EFFORT = RAW.audit && RAW.audit.effort
const FIX_MODEL = RAW.fix && !S.fix.ext && RAW.fix.model
const FIX_EFFORT = RAW.fix && RAW.fix.effort

// ── Pressure: Claude account state read once; low/exhausted steps every Claude stage down one model ──
const routeState = await agent(
  `Run exactly: ${CD}claude-code gateway router --format json --agent claude --fallback none --model sonnet. ` +
    `It prints one JSON object; state = candidates[0].state when candidates is non-empty, else the skipped[] entry whose agent ` +
    `is "claude" and its gate value, else "unknown". Never run anything else.`,
  { label: 'route-state', phase: 'Scope', schema: { type: 'object', additionalProperties: false, required: ['state'], properties: { state: { type: 'string' } } }, model: 'haiku', effort: 'low' }
).catch(() => null)
const MODEL_DOWN = { opus: 'sonnet', sonnet: 'haiku', haiku: 'haiku' }
const PRESSURE = !!(routeState && (routeState.state === 'low' || routeState.state === 'exhausted'))
const routeModel = m => (PRESSURE && MODEL_DOWN[m]) ? MODEL_DOWN[m] : m
if (PRESSURE) log(`pressure: claude account ${routeState.state} — stepping Claude stages down one model`)
else if (!routeState) log('pressure: route read failed — no downshift (fail open)')
// Passed to every child so none re-reads it; a failed read passes 'unknown' (no downshift anywhere).
const CLAUDE_STATE = routeState && typeof routeState.state === 'string' ? routeState.state : 'unknown'

const DEFAULT_DIMENSIONS = [
  { key: 'seams', prompt: 'dangling seams: callers of deleted or renamed symbols in comments, help text, docs, registration/config tables; flags parsed but never read; fields declared but never set' },
  { key: 'regressions', prompt: 'behaviour regressions: a path that used to check or emit X and silently no longer does; a doc, help or dry-run surface pointing at a removed symbol; a test rewritten to pin the broken output' },
  { key: 'tests', prompt: 'tests deleted alongside kept code, tests skipped in the range, guard tests that pin filenames the change moved, goldens that no longer match', effort: 'low' },
]
// Files mode (paths, no range) audits whole files, so the lenses are about the CODE, not the change.
// Its failure-modes lens (error handling) is not evaluate-code's references/failure-modes.md (LLM loop symptoms).
const CODE_DIMENSIONS = [
  { key: 'correctness', prompt: 'logic defects: wrong conditionals, off-by-one, nil/empty/zero-value paths that produce a wrong result or a crash, error values checked then ignored' },
  { key: 'failure-modes', prompt: 'error handling: swallowed errors, fail-open where the comment or name promises fail-closed, exec/HTTP without a timeout or context, locks that can be skipped, partial writes' },
  { key: 'contracts', prompt: 'contract drift: help text, comments or docs that disagree with the code; flags parsed but never read; fields declared but never set; exported symbols with no caller (verify with grep)', effort: 'low', hitsOnly: true },
]
let FILES_MODE = !RANGE && PATHS.length > 0
// A caller lens may be a bare prompt string or lack a key; both get key lens<i> (labels read find:1:undefined:7).
const asDims = ds => ds.map((d, i) => typeof d === 'string' ? { key: 'lens' + i, prompt: d } : { ...d, key: d.key || 'lens' + i })
const ARG_DIMS = Array.isArray(a.dimensions) && a.dimensions.length ? asDims(a.dimensions) : null
let DIMENSIONS = ARG_DIMS || (FILES_MODE ? CODE_DIMENSIONS : DEFAULT_DIMENSIONS)
// Group packages/files ≤4 per auditor so one context holds the whole group.
let GROUPS = []
for (let i = 0; i < Math.max(1, PKGS.length); i += 4) GROUPS.push(PKGS.slice(i, i + 4))

const FINDINGS_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['findings'],
  properties: {
    findings: {
      type: 'array',
      items: {
        type: 'object',
        additionalProperties: false,
        required: ['file', 'line', 'symbol', 'package', 'title', 'scenario', 'evidence', 'severity'],
        properties: {
          file: { type: 'string', description: 'repo-relative path' },
          line: { type: 'integer' },
          symbol: { type: 'string', description: 'function or symbol the defect sits in' },
          package: { type: 'string', description: 'repo-relative package dir, e.g. internal/hooks' },
          title: { type: 'string', description: 'one-sentence defect' },
          scenario: { type: 'string', description: 'concrete input/state → wrong result' },
          evidence: { type: 'string', description: 'the grep/diff/test output that proves it' },
          severity: { type: 'string', enum: ['critical', 'high', 'medium', 'low'] },
        },
      },
    },
  },
}
// One refuter per package batch (round 8: 25 per-finding refuters were the widest phase); wrong citations are re-anchored.
const VERDICTS_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['verdicts'],
  properties: {
    verdicts: {
      type: 'array',
      items: {
        type: 'object',
        additionalProperties: false,
        required: ['index', 'refuted', 'reason'],
        properties: {
          index: { type: 'integer', description: 'the finding number as listed' },
          refuted: { type: 'boolean' },
          reason: { type: 'string', description: 'quote the line that decides it' },
          file: { type: 'string', description: 'corrected repo-relative path when the citation was wrong' },
          line: { type: 'integer', description: 'corrected line when the citation was wrong' },
        },
      },
    },
  },
}
const RESULT_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['status', 'filesChanged', 'summary'], // leftUndone optional: an agent with nothing left omitted it and burned 5 retries
  properties: {
    status: { type: 'string', enum: ['done', 'partial', 'blocked'] },
    filesChanged: { type: 'array', items: { type: 'string' } },
    summary: { type: 'string' },
    leftUndone: { type: 'array', items: { type: 'string' } },
  },
}
// Sizes only, both modes: echoing a 57-file diff verbatim blew the scope agent's 64k output cap (2026-09-17).
const SCOPE_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['files'],
  properties: {
    files: {
      type: 'array',
      items: {
        type: 'object',
        additionalProperties: false,
        required: ['path', 'bytes'],
        properties: {
          path: { type: 'string', description: 'repo-relative path' },
          bytes: { type: 'integer', description: 'files mode: wc -c bytes; diff mode: (added + deleted lines) * 80 from --numstat' },
        },
      },
    },
  },
}
const relPath = p => (CWD ? String(p || '').replace(CWD + '/', '') : String(p || '')).replace(/^\.\//, '')
// A missing package defaults to the finding's own file dir; an unknown severity defaults to medium (never silently dropped).
const dirOfFile = f => relPath(f.file).split('/').slice(0, -1).join('/') || '.'
const ownerPkg = f => {
  if (!f.package) return dirOfFile(f)
  if (inPkg(f.file, f.package)) return f.package
  decisions.push(`re-routed ${relPath(f.file)}:${f.line} from ${f.package} to ${dirOfFile(f)}`)
  return dirOfFile(f)
}
const norm = f => ({
  ...f,
  package: ownerPkg(f),
  severity: ORDER[f.severity] === undefined ? (log(`unknown severity '${f.severity}' on ${f.file} → medium`), 'medium') : f.severity,
})
const NEAR = 10
// Two findings are one bug when they share a file and sit within NEAR lines or name the same symbol (as code-workers's merge).
const same = (g, f) => relPath(g.file) === relPath(f.file) && (Math.abs((g.line || 0) - (f.line || 0)) <= NEAR || (!!g.symbol && g.symbol === f.symbol))
// inPkg: a finding whose file is outside its package belongs to the file's own directory.
const inPkg = (file, p) => p === '.' ? !relPath(file).includes('/') : relPath(file).startsWith(p.replace(/\/+$/, '') + '/')
const decisions = []
const isHot = (f, hot) => hot.some(h => h.startsWith('**/') && h.endsWith('/**') ? ('/' + f).includes('/' + h.slice(3, -3) + '/') : f === h)
// Pure planner: conflict graph over tasks, greedy colouring into waves ≤cap, hotspot edits pulled out.
const planWaves = (tasks, hot, cap) => {
  const hotOf = t => [...new Set([...(t.hotspots || []), ...t.files.filter(f => isHot(f, hot))])]
  const ts = tasks.map(t => ({ ...t, hots: hotOf(t) })).map(t => ({ ...t, owns: t.files.filter(f => !t.hots.includes(f)) }))
  const hotTasks = ts.filter(t => !t.owns.length)
  const live = ts.filter(t => t.owns.length)
  const edges = []
  for (let i = 0; i < live.length; i++) for (let j = i + 1; j < live.length; j++) {
    const x = live[i], y = live[j]
    const shared = x.owns.find(f => y.owns.includes(f))
    if (shared) edges.push([x.pkg, y.pkg, 'shared file ' + shared])
    else if (x.exported && (x.importers || []).includes(y.pkg)) edges.push([x.pkg, y.pkg, y.pkg + ' imports exported ' + x.pkg])
    else if (y.exported && (y.importers || []).includes(x.pkg)) edges.push([x.pkg, y.pkg, x.pkg + ' imports exported ' + y.pkg])
  }
  const nbrs = p => edges.filter(e => e[0] === p || e[1] === p).map(e => (e[0] === p ? e[1] : e[0]))
  const order = [...live].sort((x, y) => nbrs(y.pkg).length - nbrs(x.pkg).length)
  const waves = []
  for (const t of order) {
    const n = nbrs(t.pkg)
    let w = waves.find(ws => ws.length < cap && !ws.some(o => n.includes(o.pkg)))
    if (!w) { w = []; waves.push(w) }
    w.push(t)
  }
  const out = waves.map(ws => ws.map(t => ({ pkg: t.pkg, owns: t.owns, reason: nbrs(t.pkg).length ? 'conflicts with ' + nbrs(t.pkg).join(', ') : 'no conflicts' })))
  const hotPkgs = ts.filter(t => t.hots.length)
  const hotspotTask = hotPkgs.length
    ? { owns: [...new Set(hotPkgs.flatMap(t => t.hots))], edits: hotPkgs.map(t => ({ pkg: t.pkg, files: t.hots })) }
    : null
  return { waves: out, hotspotTask, edges }
}
const planMarkdown = p => {
  const lines = ['## Tasks']
  p.waves.forEach((ws, i) => ws.forEach((t, k) => lines.push(`- [ ] Wave ${i + 1}.${k + 1} — fix ${t.pkg} findings — leaf: claude — owns: ${t.owns.join(', ')}`)))
  if (p.hotspotTask) lines.push(`- [ ] Wave ${p.waves.length + 1}.1 — apply hotspot edits for ${p.hotspotTask.edits.map(e => e.pkg).join(', ')} — leaf: claude — owns: ${p.hotspotTask.owns.join(', ')}`)
  return lines.join('\n')
}
const SCAN_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['files', 'exported', 'importers', 'hotspots'],
  properties: {
    files: { type: 'array', items: { type: 'string' }, description: 'every repo-relative file the fix will likely edit, tests included' },
    exported: { type: 'boolean', description: 'a fix changes an exported symbol or signature' },
    importers: { type: 'array', items: { type: 'string' }, description: 'repo packages that import this package' },
    hotspots: { type: 'array', items: { type: 'string' }, description: 'files from the hotspot list the fix will also edit' },
  },
}
const REFUTE_BATCH = a.refuteBatch || 6
// External audit/refute legs share one route: ≤4 in flight each (44 at once drew 429 storms); CAP is fix-wave width, not a rate.
const EXT_CAP = Math.min(CAP, 4)
const limiter = n => { let busy = 0; const q = []; const next = () => { if (busy < n && q.length) { busy++; q.shift()() } }
  return fn => new Promise((res, rej) => { q.push(() => fn().then(res, rej).finally(() => { busy--; next() })); next() }) }
const auditGate = S.audit.ext ? limiter(EXT_CAP) : fn => fn()
const refuteGate = S.refute.ext ? limiter(EXT_CAP) : fn => fn()

// ── Round loop (D2: workflow() nesting is one level only, so recursion is an in-script for-loop) ──
const rounds = []
let STARVED = false // set when a round's audit/refute legs were mostly BLOCKED
let totalMerged = 0
const PKG_COUNT = PKGS.length
for (let round = 1; round <= ROUNDS; round++) {
  // ── Scope: capture each group's diff/file sizes ONCE per round (round 1: auditors spent 31–113 turns rediscovering it) ──
  phase('Scope')
  const scopes = PRE ? [] : await parallel(GROUPS.map((g, gi) => () =>
    agent(S.scope.task(
      FILES_MODE
        ? `${CD}run ONE command: find ${g.join(' ')} -type f \\( -name '*.go' -o -name '*.py' -o -name '*.ts' -o -name '*.tsx' -o -name '*.js' -o -name '*.jsx' -o -name '*.mjs' -o -name '*.rs' -o -name '*.java' \\) ! -path '*/node_modules/*' ! -path '*/.venv/*' ! -path '*/vendor/*' ! -path '*/.git/*' ! -name '*_test.go' ! -name '*_test.py' ! -name 'test_*.py' ! -name '*.test.*' ! -name '*.spec.*' ! -path '*/tests/*' ! -path '*/__tests__/*' ! -path '*/testdata/*' -print0 | xargs -0 wc -c — and return each file as an entry ` +
          `with path and bytes. Do not read the files; the auditors cat their own chunk. At most 2 tool calls.`
        : `${CD}run ONE command: git diff --numstat ${RANGE || 'HEAD~1..HEAD'} -- ${g.join(' ') || '.'} — and return each file as an entry with path ` +
          `(the new path for a rename) and bytes = (added + deleted) * 80 (binary "-" counts as 0). Do not read the diff; the auditors run it for their chunk. At most 2 tool calls.`),
      { label: `scope:${round}:${gi}`, phase: 'Scope', schema: S.scope.schema(SCOPE_SCHEMA), ...S.scope.type, model: routeModel(S.scope.model), effort: S.scope.effort } // file listing: schema-checked, haiku is enough
    ).then(r => S.scope.done(`scope:${round}:${gi}`, r))
  ))
  // Pack files into ≤ CHUNK_CHARS chunks so no auditor sees a truncated view and agent count follows diff size.
  const CHUNK_CHARS = a.chunkChars || 50000
  const chunks = []
  if (!PRE) GROUPS.forEach((g, gi) => {
    const files = scopes[gi] ? scopes[gi].files : []
    let cur = { group: g, gi, files: [], chars: 0 }
    for (const f of files) {
      const size = f.bytes || 0
      if (cur.files.length && cur.chars + size > CHUNK_CHARS) { chunks.push(cur); cur = { group: g, gi, files: [], chars: 0 } }
      cur.files.push(f); cur.chars += size
    }
    if (cur.files.length) chunks.push(cur)
    else if (!scopes[gi]) chunks.push({ group: g, gi, files: [], chars: 0 }) // scope failed: the auditor falls back to git/find itself
    log(`scope ${gi}: ${files.length} files, ~${files.reduce((n, f) => n + (f.bytes || 0), 0)} bytes`)
  })
  if (!PRE && FILES_MODE && !chunks.length) {
    rounds.push({ error: 'code-analyzer: no source files under ' + PATHS.join(', '), exit: 'error', round })
    break
  }
  log(`${chunks.length} chunk(s) of ≤${CHUNK_CHARS} chars → ${DIMENSIONS.filter(d => !d.hitsOnly).length * chunks.length} first-pass auditors`)

  // ── Find → Refute as ONE pipeline: a chunk's findings are refuted while other chunks still audit ──
  // hitsOnly lenses (contracts) run in a second pass over just the files the first pass confirmed a finding in:
  // round 9–10 measured contracts at a third of Find while yielding mostly lows.
  const mkUnits = (dims, cs, pass) => dims.flatMap(d => cs.map((c, ci) => ({ dim: d, group: c.group, key: `${d.key}:${pass}${ci}`, chunk: c })))
  const primaryDims = DIMENSIONS.filter(d => !d.hitsOnly)
  const hitDims = DIMENSIONS.filter(d => d.hitsOnly)
  log('auditing ' + TARGET + ' — ' + primaryDims.length + ' lens(es) × ' + chunks.length + ' chunk(s)' + (hitDims.length ? ', then ' + hitDims.map(d => d.key).join('/') + ' over flagged files' : ''))
  const seen = [...PRIOR] // proximity-merged across chunks, lenses and rounds
  let mergedCount = 0
  const refuted = []
  const unverified = []
  let refuteCalls = 0
  let auditorCount = 0
  const findStage = u => {
    auditorCount++
    const prompt =
      `Read-only audit of ${TARGET}${IN}. Packages: ${u.group.join(', ') || 'all touched'}. Lens: ${u.dim.prompt}.\n` +
      (FILES_MODE
        ? `First tool call: cat the files listed below (one call, all of them). Judge from that; open another file only to check a caller or a test (budget 8 tool calls). `
        : `First tool call: ${CD}git diff ${RANGE || 'HEAD~1..HEAD'} -- <the files listed below> (one call, all of them). Judge from that diff; open a file only to check a caller or a test named in it (budget 8 tool calls). `) +
      `Report only defects you can evidence with a grep, diff or test run — no style nits. ${SHARED_TREE}\n` +
      `Method: ~/.claude/skills/evaluate-code/SKILL.md (report only findings on changed code you verified; >80% confidence filter)\n\n` +
      (u.chunk.files.length
        ? `Files: ${u.chunk.files.map(f => f.path).join(' ')}\n\n`
        : `(the scope agent returned nothing — ${FILES_MODE ? 'read the files under ' + u.group.join(' ') : 'run git diff --stat ' + (RANGE || 'HEAD~1..HEAD') + ' -- ' + (u.group.join(' ') || '.')} yourself)`)
    return auditGate(() => agent(S.audit.task(prompt),
      { label: `find:${round}:${u.key}`, phase: 'Find', schema: S.audit.schema(FINDINGS_SCHEMA), ...S.audit.type, model: routeModel(S.audit.model), effort: AUDIT_EFFORT || u.dim.effort || 'low' }
    )).then(r => S.audit.done(`find:${round}:${u.key}`, r))
  }
  const refuteStage = (found, u) => {
    const fresh = (found ? found.findings : []).filter(f => {
      if (seen.some(g => same(g, f))) return false
      seen.push(f)
      return true
    })
    mergedCount += fresh.length
    // Batched per PACKAGE, ≤ REFUTE_BATCH findings each (round 11: per-file refuters grew to 24 agents / 7.9M reads).
    const byPkg = new Map()
    for (const f of fresh) {
      const k = ownerPkg(f)
      byPkg.set(k, [...(byPkg.get(k) || []), f])
    }
    const batches = []
    for (const [pkg, all] of byPkg) for (let i = 0; i < all.length; i += REFUTE_BATCH) batches.push([pkg, all.slice(i, i + REFUTE_BATCH), i / REFUTE_BATCH])
    return Promise.all(batches.map(([pkg, fs, bi]) => {
      refuteCalls++
      const list = fs.map((f, i) => `${i}. ${f.title}\n   At: ${f.file}:${f.line}\n   Scenario: ${f.scenario}\n   Evidence: ${f.evidence}`).join('\n')
      const brief = `Try to REFUTE each of these ${fs.length} audit finding(s) in ${pkg} against the real tree${IN} (read-only); one verdict per index.\n${list}\n\n` +
        `Refuted means: the code path does not do what is claimed, the scenario cannot occur, or it is already handled; quote the line that decides it. ` +
        `Default to refuted=true when the evidence does not hold up. A TRUE claim with a WRONG file or line is NOT refuted: set refuted=false and give the correct file and line. ` +
        // A "no such verb" finding once survived and its fixer rewrote a correct doc; the verb was an alias spelling.
        `A claim that a verb, flag, symbol or file does NOT exist is refuted unless a tree-wide grep (alias, registration and config tables included) AND the repo's own test or CLI output both come back empty.`
      const label = `refute:${round}:${pkg.split('/').pop()}:${bi}`
      return refuteGate(() => agent(S.refute.task(brief), { label, phase: 'Refute', schema: S.refute.schema(VERDICTS_SCHEMA), ...S.refute.type, model: routeModel(S.refute.model), effort: S.refute.effort })).then(v => {
        v = S.refute.done(label, v)
        const verdicts = v && Array.isArray(v.verdicts) ? v.verdicts : []
        return fs.map((f, i) => {
          if (!v) { unverified.push({ ...f, reason: 'refuter returned no result' }); return null }
          const vd = verdicts.find(x => x.index === i)
          if (!vd) { unverified.push({ ...f, reason: 'refuter gave no verdict for this finding' }); return null }
          if (vd.refuted) { refuted.push({ ...f, reason: vd.reason }); return null }
          return norm({ ...f, file: vd.file || f.file, line: vd.line || f.line, lens: u.dim.key })
        })
      })
    })).then(groups => groups.flat())
  }
  const collect = results => results.filter(Boolean).flat().filter(Boolean)
  const confirmed = PRE ? PRE.map(norm) : collect(await pipeline(mkUnits(primaryDims, chunks, ''), findStage, refuteStage))
  if (!PRE && hitDims.length) {
    const flagged = new Set(confirmed.map(f => relPath(f.file)))
    const hitChunks = chunks.map(c => ({ ...c, files: c.files.filter(f => flagged.has(relPath(f.path))) })).filter(c => c.files.length)
    log(`hits-only pass: ${hitChunks.length} chunk(s) over ${flagged.size} flagged file(s)`)
    if (hitChunks.length) confirmed.push(...collect(await pipeline(mkUnits(hitDims, hitChunks, 'h'), findStage, refuteStage)))
  }
  confirmed.sort((x, y) => ORDER[x.severity] - ORDER[y.severity])
  const toFix = confirmed.filter(f => ORDER[f.severity] <= MIN_FIX)
  const deferred = confirmed.filter(f => ORDER[f.severity] > MIN_FIX)
  totalMerged += mergedCount
  const metrics = { scopers: PRE ? 0 : GROUPS.length, chunks: chunks.length, auditors: auditorCount, refuters: refuteCalls, confirmed: confirmed.length, refuted: refuted.length, deferred: deferred.length }
  log(`round ${round}/${ROUNDS}: merged ${mergedCount}, confirmed ${confirmed.length}, refuted ${refuted.length}, below the fix floor ${deferred.length}, unverified ${unverified.length}`)

  const base = { confirmed, refuted, unverified, deferred, round }
  // More than half of a stage's external legs BLOCKED means the round saw too little to call it clean.
  const legs = p => COWORKERS.filter(c => c.label.startsWith(p))
  const starved = [['audit', `find:${round}:`], ['refute', `refute:${round}:`]].map(([st, p]) => [st, legs(p)])
    .filter(([, ls]) => ls.filter(c => c.status === 'BLOCKED').length * 2 > ls.length)
  if (starved.length) {
    for (const [st, ls] of starved) decisions.push(`round ${round}: ${st} ${ls.filter(c => c.status === 'BLOCKED').length}/${ls.length} external legs BLOCKED — exit blocked, not dry`)
    STARVED = true // coverage gap: still fix what was confirmed, but never report the run clean
    if (!FIX || !toFix.length) { rounds.push({ ...base, fixed: [], gate: null, exit: 'blocked', metrics }); break }
  }
  if ((!FIX && !PART) || !toFix.length) { rounds.push({ ...base, fixed: [], gate: null, exit: FIX ? 'dry' : 'no-fix', metrics }); break }
  // Same findings as an earlier round (PRIOR, accumulated across rounds): report, do not re-fix forever.
  if (PRIOR.length && toFix.every(f => PRIOR.some(g => same(g, f)))) { rounds.push({ ...base, fixed: [], gate: null, exit: 'stalled', metrics }); break }

  // ── Scan → Plan: dependency scan per package, then a deterministic wave partition ──
  const byPkg = new Map()
  for (const f of toFix) byPkg.set(f.package, [...(byPkg.get(f.package) || []), f])
  phase('Scan')
  const scans = await parallel([...byPkg.entries()].map(([pkg, fs]) => () => agent(S.depscan.task(
    `Read-only dependency scan of ${pkg}${IN}. A fixer will repair these findings:\n${fs.map(f => `- ${relPath(f.file)}:${f.line} ${f.title}`).join('\n')}\n` +
      `Return files = every file the fix will likely edit (tests included), exported = whether it changes an exported symbol or signature, ` +
      `importers = repo packages importing ${pkg} (run once: ${CD}go list -f '{{.ImportPath}}: {{join .Imports " "}}' ./... and keep repo packages only, as repo-relative dirs), ` +
      `hotspots = those of ${HOT.join(', ')} the fix must also edit. At most 5 tool calls. ${SHARED_TREE}`),
    { label: `depscan:${round}:${pkg.split('/').pop()}`, phase: 'Scan', schema: S.depscan.schema(SCAN_SCHEMA), ...S.depscan.type, model: routeModel(S.depscan.model), effort: S.depscan.effort }
  ).then(r => S.depscan.done(`depscan:${round}:${pkg.split('/').pop()}`, r)).catch(() => null)))
  const planTasks = [...byPkg.entries()].map(([pkg, fs], i) => {
    const sc = scans[i]
    if (!sc) decisions.push(`depscan failed for ${pkg}: files taken from the findings, no importer edges`)
    const files = [...new Set([...(sc ? sc.files : []), ...fs.map(f => f.file)].map(relPath))]
    return { pkg, files, exported: !!(sc && sc.exported), importers: sc ? sc.importers.map(relPath) : [], hotspots: sc ? sc.hotspots.map(relPath) : [] }
  })
  phase('Plan')
  const partition = planWaves(planTasks, HOT, CAP)
  const planMd = planMarkdown(partition)
  const sizes = partition.waves.map(w => w.length)
  log(`plan: ${sizes.length} wave(s) [${sizes.join(',')}], ${partition.edges.length} edge(s)`)
  decisions.push(`partition: ${sizes.length} wave(s), max parallel ${Math.max(0, ...sizes)}, ${partition.edges.length} conflict edge(s), ${partition.hotspotTask ? partition.hotspotTask.owns.length : 0} hotspot file(s) pulled out`)
  if (!FIX) { rounds.push({ ...base, fixed: [], gate: null, partition, plan: planMd, exit: 'partition', metrics }); break }

  // ── Fix: waves in sequence, tasks of a wave in parallel (per task ADK Sequential: implementer, then test agent), then the hotspot task ──
  phase('Fix')
  const fixTask = (pkg, fs, owns, hotspot) => {
    const list = fs.map(f => `- ${f.file}:${f.line} [${f.severity}] ${f.title}\n  scenario: ${f.scenario}\n  evidence: ${f.evidence}`).join('\n')
    const files = [...new Set(fs.map(f => relPath(f.file)))]
    const own = `You own ONLY: ${owns.join(', ')}. Never edit any other file; report it in leftUndone instead.`
    const ctx = `Confirmed defects in ${pkg}${IN}:\n${list}\n${own}\n${SHARED_TREE}`
    // Opus only where a high/critical sits; the evidence rides in the prompt and the files are read in ONE call.
    const hot = fs.some(f => ORDER[f.severity] <= ORDER.high)
    const testsNeeded = !hotspot && TESTS && fs.some(f => !f.noTest && SRC_RE.test(relPath(f.file))) // docs-only or all-noTest: no test agent
    // Test agent runs after the implementer and gets its result, so it asserts against code that has landed.
    return (async () => {
      const implTask = `${ctx}\nYou are the IMPLEMENTER: fix every listed defect in ${hotspot ? 'the files you own (hotspot edits other fixers deferred to you)' : `non-test files of ${pkg} you own; never test files or fixtures (${TEST_GLOBS})`}. ` +
        `The evidence above is already verified — do not re-audit. First tool call: cat ${files.join(' ')} (one call). ` +
        `Budget ~15 tool calls; then run the repo's formatter and linter on the files you touched (whichever it has: make fmt/lint, gofmt + go vet, ruff, eslint) and report per defect.\n` +
        `Before a behavior change write TASK: code does X; check expects Y; spec says Z (Y vs Z disagree = stop, report).\n` +
        `After a defect fix write ISSUE: searched P, found N.\n` +
        `Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 4-6)\n` +
        `Put the TASK/ISSUE facts as plain sentences in your summary.`
      const impl = S.fix.done(`fix:${round}:${pkg}`, await agent(S.fix.task(implTask),
        { label: `fix:${round}:${pkg}`, phase: 'Fix', schema: S.fix.schema(RESULT_SCHEMA), ...S.fix.type, model: routeModel(FIX_MODEL || (hot ? 'opus' : 'sonnet')), effort: FIX_EFFORT || (hot ? 'high' : 'medium') }))
      const implBlocked = !impl || impl.status === 'blocked'
      const notDone = !impl || impl.status !== 'done' // a gate over a fix that did not finish proves nothing: red
      const testTask = () => `${ctx}\nImplementer result (already landed on disk): status ${impl.status}; files changed: ${(impl.filesChanged || []).join(', ') || 'none'}; summary: ${impl.summary}\nYou are the TEST agent (the implementer has finished): only test files and fixtures (${TEST_GLOBS}) for ${pkg} that you own. ` +
        `Method: ~/.claude/agents/copilot.md → TEST. ` +
        `Read the cited files and their tests in one call; repair tests the fixes break, add one regression test per defect pinning its scenario ` +
        `(none for a comment-only or dead-code fix), run only the tests for ${pkg}, and report. Budget ~20 tool calls.`
      const tests = !testsNeeded || implBlocked ? null : S.test.done(`test:${round}:${pkg}`, await agent(S.test.task(testTask()),
        { label: `test:${round}:${pkg}`, phase: 'Fix', schema: S.test.schema(RESULT_SCHEMA), ...S.test.type, model: routeModel(S.test.model), effort: S.test.effort }))
      return { package: pkg, pkg, findings: fs.length, impl, tests, test: tests, blocked: notDone, notDone }
    })()
  }
  const fixed = []
  for (const wave of partition.waves) {
    const rs = await parallel(wave.map(t => () => fixTask(t.pkg, byPkg.get(t.pkg), t.owns, false)))
    fixed.push(...rs.map((r, i) => r || { package: wave[i].pkg, blocked: true, error: 'fix stage failed' }))
  }
  if (partition.hotspotTask) {
    const ht = partition.hotspotTask
    const fs = ht.edits.flatMap(e => byPkg.get(e.pkg))
    const r = await fixTask('hotspots', fs, ht.owns, true).catch(() => null)
    fixed.push(r || { package: 'hotspots', blocked: true, error: 'hotspot fix failed' })
  }

  // ── Gate (ADK LoopAgent via the saved green-keeper-loop workflow; a.gate passes through — green-keeper-loop auto-detects) ──
  phase('Gate')
  const pkgs = [...byPkg.keys()]
  const gate = await workflow('green-keeper-loop', { claudeState: CLAUDE_STATE, gate: a.gate, cwd: CWD || undefined, scope: pkgs, maxIterations: MAX_ITER, profile: a.profile, provider: a.provider, ...(RAW.gate ? { steps: { fix: RAW.gate } } : {}) })

  const metricsOut = { ...metrics, fixPairs: byPkg.size, gateIterations: gate ? gate.iterations : 0 }
  const out = { ...base, fixed, gate, partition, plan: planMd, metrics: metricsOut }
  if (!gate || gate.status !== 'green' || out.fixed.some(x => x.blocked)) { rounds.push({ ...out, exit: 'blocked' }); break }
  const touched = [...new Set([
    ...out.fixed.flatMap(x => [...((x.impl && x.impl.filesChanged) || []), ...((x.tests && x.tests.filesChanged) || [])]),
    ...(gate.filesChanged || []),
  ].map(relPath))].filter(p => SRC_RE.test(p) && !TEST_RE.test(p))
  if (!touched.length) { rounds.push({ ...out, exit: 'nothing-changed' }); break }
  if (round >= ROUNDS) { rounds.push({ ...out, exit: 'capped' }); break }
  if (typeof budget !== 'undefined' && budget && budget.total && budget.remaining() < 150000) {
    decisions.push(`re-audit round ${round + 1} skipped: budget under 150k tokens remaining`)
    log(`re-audit skipped: low budget`)
    rounds.push({ ...out, exit: 'capped' })
    break
  }
  log(`round ${round + 1}/${ROUNDS}: re-auditing ${touched.length} file(s) the fix touched`)
  rounds.push(out)
  // Next round audits exactly what this round's fix touched — PKGS/GROUPS are just the touched file list, chunked.
  PRIOR = [...PRIOR, ...toFix]
  PATHS = touched
  PKGS = touched
  FILES_MODE = true
  GROUPS = []
  for (let i = 0; i < Math.max(1, PKGS.length); i += 4) GROUPS.push(PKGS.slice(i, i + 4))
  DIMENSIONS = ARG_DIMS || CODE_DIMENSIONS
  TARGET = 'the code under ' + PATHS.join(', ')
  PRE = null
}

const lastRound = rounds[rounds.length - 1] || { exit: 'error', round: 0 }
const allConfirmed = rounds.flatMap(r => r.confirmed || [])
const unverifiedAll = rounds.flatMap(r => r.unverified || [])
const fixedAll = rounds.flatMap(r => r.fixed || [])
// Red: a package whose fixer is not done, or every fixed package of a round whose gate is not green.
const red = [...new Set(rounds.flatMap(r => (r.fixed || []).filter(x => x.notDone || x.blocked || (r.gate && r.gate.status !== 'green')).map(x => x.package)))]
for (const r of rounds) {
  for (const x of r.fixed || []) decisions.push(`${x.notDone ? 'fix NOT done (' + (x.impl ? x.impl.status : 'no result') + ') for ' : 'fixed '}${x.findings || 0} finding(s) in ${x.package}`)
  if (r.gate) decisions.push(`round ${r.round}: gate ${r.gate.status}`)
}
if (!FIX) decisions.push('review only: fix=false')
const lastPart = [...rounds].reverse().find(r => r.partition)
const partitionOut = lastPart ? lastPart.partition : null
const planOut = lastPart ? lastPart.plan : ''
if (unverifiedAll.length) decisions.push(`${unverifiedAll.length} finding(s) unverified: refuter returned nothing`)
const report = [`# code-analyzer: ${allConfirmed.length} confirmed finding(s) over ${rounds.length} round(s)`,
  ...allConfirmed.map(c => `- **${c.severity}** \`${relPath(c.file)}:${c.line}\` (${c.symbol || '?'}) — ${c.title}`),
  '', `Fixed: ${fixedAll.map(f => f.package + ' (' + (f.findings || 0) + ')').join(', ') || 'none'}. Red: ${red.join(', ') || 'none'}.`,
  ...(unverifiedAll.length ? [`Unverified (refuter failed): ${unverifiedAll.map(u => relPath(u.file) + ':' + u.line).join(', ')}.`] : []),
  ...(planOut ? ['', planOut] : [])].join('\n')
return {
  ...lastRound, exit: STARVED && lastRound.exit !== 'error' ? 'blocked' : lastRound.exit, round: lastRound.round, rounds,
  route: 'code-analyzer', packages: PKG_COUNT, merged: totalMerged,
  dropped: rounds.flatMap(r => (r.refuted || []).map(f => ({ file: relPath(f.file), line: f.line, symptom: f.title }))),
  unverified: unverifiedAll, fixed: fixedAll, red, decisions, report, partition: partitionOut, coworkers: COWORKERS,
}
