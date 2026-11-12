export const meta = {
  name: 'code-analyzer',
  description: 'Audit a change set: parallel scoped auditors → refute each finding → per-package implementer, then test repair → gate loop, every editing stage numstat-measured',
  whenToUse:
    'USE: audit before a merge or after a refactor; review-only with fix:false. CALL: Workflow({scriptPath: "~/.claude/workflows/code-analyzer.js", args: {cwd, paths: [...] | range: "main..HEAD" | findings}}). OPTIONAL: maxAgents (hard cap on agents per run, default 200), fix (default true), partition (with fix:false: return the wave plan, no edits), cap (wave width, default 8), hotspots (extra single-owner files), minFix, rounds, dimensions (replaces the lenses), focus (appended to every auditor lens), tests, gate (command override), maxIterations (gate loop cap), prior (earlier findings to skip), chunkChars (chunk size), refuteBatch (findings per skeptic), kind (audit default; consolidate swaps the lenses for duplicates, dead code, legacy and stale tests), lineBudget (shrink|neutral|free; default by kind: refactor neutral, consolidate shrink, else free; an over-budget fix gets one shrink-or-justify pass, still over exits review), steps {scope, audit, refute, fix, test, gate, depscan} = {agent?, model?, effort?, profile?, provider?} per stage (agent claude|codex|copilot|gemini|opencode; external stages run on their pins, or args/steps profile|provider, and fall back on 429/auth; each reports ran on or BLOCKED). OPTIONAL: profile, provider (run-wide override for external stages). An unknown arg is refused at launch. Test stages repair what the fixes broke and write nothing new; every other test comes back in proposedTests. COST: about 8 agent slots per run, so one run at a time. RETURNS {exit, route, packages, merged, confirmed, dropped, unverified, fixed, red (fix not done, or the package\'s last gate not green), decisions, report, partition, lines (numstat per stage, per package and total, production vs test), proposedTests, over, gates} plus refuted, rounds. Details: saved-workflows.md.',
  phases: [
    { title: 'Scope', detail: 'one cheap agent per package group returns per-file sizes; the script packs them into ≤50 KB chunks' },
    { title: 'Find', detail: 'one auditor per (dimension × chunk) reading its chunk in-context — agent count scales with diff size' },
    { title: 'Refute', detail: 'one skeptic per package batch (≤6 findings); re-anchors wrong citations, defaults to refuted when the evidence is thin' },
    { title: 'Scan', detail: 'one read-only agent per package with fixable findings: files, exported, importers, hotspots' },
    { title: 'Plan', detail: 'script-only conflict graph, greedy colouring into waves ≤cap; hotspot edits go to one final task' },
    { title: 'Fix', detail: 'waves in sequence, tasks of a wave in parallel; per package: implementer (opus only for highs), then test repair (new tests proposed, never written)' },
    { title: 'Gate', detail: 'in-script stageGate (shared stages, check-confirmed) over the touched packages, then re-audits what it touched for up to rounds rounds' },
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
const HOTSPOTS = ['internal/schemas/ledger_test.go', 'skills/subagent-workflows/references/saved-workflows.md', 'settings.json', 'go.mod', 'go.sum', 'Makefile', '**/testdata/**']
const HOT = [...HOTSPOTS, ...(Array.isArray(a.hotspots) ? a.hotspots.filter(Boolean) : [])]
// Wave width: args.cap, bounded by the dispatch hook's 24 per account.
const CAP = Math.min(Number.isInteger(a.cap) && a.cap > 0 ? a.cap : 8, 24)
// tests:false skips the per-package test agent for every package (no test agent).
if (a.tests !== undefined && typeof a.tests !== 'boolean') return ERR('args.tests must be a boolean')
const TESTS = a.tests !== false
const MAX_ITER = Number.isInteger(a.maxIterations) && a.maxIterations > 0 ? a.maxIterations : 3
// Rounds re-audit exactly the non-test source the previous round's fix touched, until dry/stalled/capped (D2: in-script loop, not self-recursion).
const ROUNDS = Number.isInteger(a.rounds) && a.rounds > 0 ? a.rounds : 2
if (a.maxAgents !== undefined && !(Number.isInteger(a.maxAgents) && a.maxAgents > 0)) return ERR('args.maxAgents must be a positive integer')
const MAX_AGENTS = a.maxAgents || 200
// Every agent() but the shared stage block goes through ag: past MAX_AGENTS it resolves null and the run exits 'error'.
let AGENTS = 0, LIMIT_HIT = false
const ag = (p, o) => { if (++AGENTS > MAX_AGENTS) { LIMIT_HIT = true; return Promise.resolve(null) } return (0, agent)(p, o) }
const LIMIT_MSG = `code-analyzer: agent limit ${MAX_AGENTS} reached (args.maxAgents); later stages did not run`
// Prior findings as {file, line, symbol}; a string 'file:line' is accepted. Matched by proximity (same), not wording.
let PRIOR = (Array.isArray(a.prior) ? a.prior : []).map(p => typeof p === 'string' ? { file: p.replace(/:\d+$/, ''), line: +(p.match(/:(\d+)$/) || [0, 0])[1], symbol: '' } : p)
// Clean break: the per-stage args moved into steps.
const REPLACED = { auditModel: 'audit', refuteModel: 'refute', refuters: 'refute', profiles: 'refute' }
for (const k of Object.keys(REPLACED)) {
  if (a[k] !== undefined) return ERR(`${k} was replaced by steps.${REPLACED[k]} (plans/2026-11-03-workflow-steps.md)`)
}
const ARGS = ['cwd', 'paths', 'packages', 'findings', 'range', 'fix', 'partition', 'hotspots', 'cap', 'tests', 'maxIterations', 'rounds', 'prior', 'minFix', 'steps', 'profile', 'provider',
  'dimensions', 'focus', 'chunkChars', 'refuteBatch', 'gate', 'kind', 'lineBudget', 'maxAgents']
if (stArgs(a, ARGS)) return ERR(stArgs(a, ARGS))
if (a.kind !== undefined && !['feature', 'refactor', 'fix', 'audit', 'consolidate'].includes(a.kind)) return ERR('args.kind must be feature|refactor|fix|audit|consolidate')
if (a.lineBudget !== undefined && !['shrink', 'neutral', 'free'].includes(a.lineBudget)) return ERR('args.lineBudget must be shrink|neutral|free')
const JOB = a.kind || 'audit'
const LINE_BUDGET = a.lineBudget || (JOB === 'refactor' ? 'neutral' : JOB === 'consolidate' ? 'shrink' : 'free')
const SRC_RE = /\.(go|py|ts|tsx|js|jsx|mjs|rs|java)$/
const TEST_RE = /(_test\.(go|py)$|\.(test|spec)\.[a-z]+$|(^|\/)test_[^/]*\.py$|(^|\/)(tests?|__tests__|testdata|fixtures)\/)/
// Fix-phase severity floor: default low fixes every finding (decision #54); minFix:'medium' leaves lows unfixed.
const ORDER = { critical: 0, high: 1, medium: 2, low: 3 }
const MIN_FIX = ORDER[a.minFix] !== undefined ? ORDER[a.minFix] : ORDER.low
let TARGET = RANGE ? 'the diff of ' + RANGE : 'the code under ' + PATHS.join(', ')
const SHARED_TREE =
  'Shared live tree: never git stash|checkout|reset|restore|clean|commit, never make build|install|test|audit, never edit settings.json. ' +
  (CWD ? 'cwd: ' + CWD + ' (external agents: ask --workdir ' + CWD + '). Prefix every shell command with `cd ' + CWD + ' &&` (each Bash call starts in the session dir; the copilot shell allows a cd inside its roots). ' : '') + 'Comments 1 line ≤120 chars.'

// ── stepOf: copied verbatim into code-analyzer, green-keeper-loop, code-evaluator, code-workers, code-engineer ──
// stepOf(steps, name, defaults, {allowed, external, profile, provider}) -> stage; claude in-context, others via `agents ask`.
const COWORKERS = []
// Unknown stage/field backstop only; value and route checks run in the dispatch-effort hook (workflowlint.go).
const stepOf = (steps, name, defaults, opt) => {
  const S_FIELDS = ['agent', 'model', 'effort', 'profile', 'provider']
  const o = opt || {}
  const allowed = o.allowed || []
  const d = defaults || {}
  if (steps !== undefined && (!steps || typeof steps !== 'object' || Array.isArray(steps))) throw new Error('steps must be an object')
  for (const k of Object.keys(steps || {})) if (!allowed.includes(k)) throw new Error(`steps.${k}: unknown stage (allowed: ${allowed.join('|')})`)
  const where = `steps.${name}`
  const v = (steps || {})[name] === undefined ? {} : steps[name]
  if (!v || typeof v !== 'object' || Array.isArray(v)) throw new Error(`${where} must be an object`)
  for (const k of Object.keys(v)) if (!S_FIELDS.includes(k)) throw new Error(`${where}.${k}: unknown field (allowed: ${S_FIELDS.join('|')})`)
  const who = v.agent !== undefined ? v.agent : 'claude'
  const effort = v.effort !== undefined ? v.effort : d.effort || 'medium'
  if (who === 'claude') {
    const model = v.model !== undefined ? v.model : d.model || 'sonnet'
    const same = (_label, r) => r
    return { agent: 'claude', ext: false, model, effort, type: d.agentType ? { agentType: d.agentType } : {}, task: (p) => p, schema: (s) => s, done: same, text: same }
  }
  if (!o.external) throw new Error(`${where}.agent: ${who} is not wired in this workflow yet (claude only)`)
  // no profile/provider: Go routes the agent's pin; a named one leads, ladder after.
  const profile = v.profile !== undefined ? v.profile : o.profile || ''
  const provider = v.provider !== undefined ? v.provider : o.provider || ''
  const edit = d.tools === 'edit'
  const mode = edit ? '--tools' : who === 'copilot' ? '--read-only' : '--tools'
  const ask = `claude-code ${who} ask --fallback agents --format json ${mode} --effort ${effort}` + (profile ? ` --profile ${profile}` : '') +
    (provider ? ` --provider ${provider}` : '') + (v.model ? ` --model ${v.model}` : '')
  const F = { ran_on: { type: 'string' }, hops: { type: 'integer' }, coworker: { type: 'string', enum: ['ok', 'BLOCKED'] }, coworker_error: { type: 'string' } }
  const R = ['ran_on', 'coworker']
  const record = (label, r) => {
    const ranOn = (r && r.ran_on) || ''
    // the envelope's line (wire.RanOn), or a reworded one naming the agent and a route (account·profile, profile:model or model id)
    const canon = /^ran on \S+ · .+ · fallback (?:none|from )/.test(ranOn)
    const reworded = /^(?:ran on )?(?:codex|copilot|gemini|opencode)\b/.test(ranOn) && (/\S[·:][A-Za-z0-9]/.test(ranOn) || /\b[A-Za-z]+-[\w.-]*\d/.test(ranOn))
    const failed = /\b(?:unavailable|failed|blocked|no execution|not run)\b/i.test(ranOn)
    const ok = !!(r && r.coworker === 'ok' && (canon || reworded) && !failed)
    if (ok && !canon) log(`coworkers ${label}: ran_on reworded by the relay: ${ranOn}`)
    const why = ok ? '' : (r && r.coworker_error) || 'no ran_on in the result'
    COWORKERS.push({ stage: name, label, agent: who, status: ok ? 'ok' : 'BLOCKED', ran_on: ok ? r.ran_on : '', hops: (r && r.hops) || 0, error: why })
    log(`coworkers ${label}: ${ok ? (/^ran on /.test(r.ran_on) ? '' : 'ran on ') + r.ran_on + ' (hop ' + ((r && r.hops) || 0) + ')' : who + ' BLOCKED: ' + why}`)
    return ok
  }
  return {
    agent: who, ext: true, model: 'sonnet', effort: 'low', type: { agentType: who }, profile, provider,
    task: (p) => `Agent: ${who}\nRun the task below with \`${ask} --attach <file>\`: Write the task text to a NEW file from \`mktemp "${'$'}{TMPDIR:-/tmp}/${who}-task.XXXXXX"\` (never a fixed name: parallel legs overwrite each other), never in the repo; Read that empty file before you Write it (Write refuses an existing file it has not read), attach that exact path, and never do it in-context.` +
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
// ── stages: copied verbatim into code-analyzer, green-keeper-loop, code-workers, code-engineer ──
// Shared stages, run in-script so workflow() nests one level; ctx = {cwd, cd, rules, S, model?, gateMax?, gate?, kind?, lineBudget?, seams?}.
const ST_NEAR = 10
const ST_GATE_MAX = 2
const ST_TAIL = 4000 // chars of the initial gate failure carried into every fixer iteration
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
const ST_CHECK = { type: 'object', properties: { green: { type: 'boolean' }, output: { type: 'string' } }, required: ['green', 'output'] }
const ST_SIMP = { type: 'object', properties: { applied: ST_LIST, flagged: ST_LIST, deltaLines: { type: 'integer' }, status: { type: 'string', enum: ['done', 'clean', 'partial', 'blocked'] } }, required: ['applied', 'flagged', 'deltaLines', 'status'] }
const ST_REVF = { type: 'object', properties: { findings: { type: 'array', items: { type: 'object', properties: { file: { type: 'string' }, line: { type: 'integer' }, symbol: { type: 'string' }, symptom: { type: 'string' }, severity: { type: 'string', enum: ['critical', 'high', 'medium', 'low'] } }, required: ['file', 'line', 'symbol', 'symptom', 'severity'] } } }, required: ['findings'] }
const ST_LOOP = {
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
const ST_DETECT = {
  type: 'object',
  additionalProperties: false,
  required: ['gate', 'source'],
  properties: {
    gate: { type: 'string', description: 'exact command, empty when nothing matches' },
    source: { type: 'string' },
  },
}
// Measured run state: numstat rows per editing stage, proposed tests, over-budget fixes, seam needs, gate results in order.
const ST_LINES = []
const ST_PROPOSED = []
const ST_OVER = []
const ST_SEAMS = []
const ST_GATES = []
const ST_TEST_RE = /(_test\.go$|\.test\.[jt]sx?$|(^|\/)(tests|testdata)\/)/
const ST_BUDGET = { shrink: 0, neutral: 20 } // max production net lines per package; free has no entry
const stOver = (ctx, net) => ST_BUDGET[ctx.lineBudget] !== undefined && net > ST_BUDGET[ctx.lineBudget]
const ST_NUM = { type: 'object', properties: { rows: { type: 'array', items: { type: 'object', properties: { file: { type: 'string' }, added: { type: 'integer' }, deleted: { type: 'integer' } }, required: ['file', 'added', 'deleted'] } } }, required: ['rows'] }
const ST_FIXW = { ...ST_WORK, properties: { ...ST_WORK.properties, seams: { type: 'array', items: { type: 'object', properties: { file: { type: 'string' }, need: { type: 'string' } }, required: ['file', 'need'] } } } }
const ST_TESTW = { ...ST_WORK, properties: { ...ST_WORK.properties, proposed: { type: 'array', items: { type: 'object', properties: { case: { type: 'string' }, reason: { type: 'string' }, runtime: { type: 'string' } }, required: ['case', 'reason', 'runtime'] } } } }
const ST_PROPOSE = ' Every other test worth adding (a regression test for a bug fix, a pin for a cost fix) stays unwritten: put it in `proposed` as {case (input → expected), reason (what it guards), runtime (expected, e.g. "<5ms")}.' +
  ' Any test you write must run in milliseconds: no sleep, real network, vault, daemon or spawned binary; each under complexity 10.'
// the numstat reader is always a local cheap leaf: an external relay adds a hop and a second self-report
const ST_MEASURER = { agent: 'claude', ext: false, model: 'haiku', effort: 'low', type: {}, task: (p) => p, schema: (s) => s, done: (_l, r) => r }
// stSnap: {file: {a, d}} from `git diff HEAD --numstat` plus untracked files, null when the reader fails.
async function stSnap(ctx, label, paths) {
  const ps = paths.filter((p) => p && !/\s/.test(p)).join(' ') || '.'
  const r = await stageAsk(ctx, ST_MEASURER, 'numstat:' + label, 'Measure', 'Read-only: edit nothing. Run exactly `' + (ctx.cwd ? 'cd ' + ctx.cwd + ' && ' : '') + 'git diff HEAD --no-ext-diff --no-textconv --numstat -- ' + ps +
    '` and `git ls-files --others --exclude-standard -- ' + ps + '`. Return one row per numstat line verbatim (file, added, deleted; a binary "-" is 0) and one row per untracked file with added = its `wc -l` count, deleted 0. Run nothing else.', ST_NUM)
  if (!r) return null
  const m = {}
  for (const x of r.rows || []) m[stRel(ctx, x.file)] = { a: x.added || 0, d: x.deleted || 0 }
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
// stageGate: an optional cheap check, then up to o.max fixer iterations over o.scope; exit green|stalled|capped|error.
async function stageGate(ctx, o) {
  const tag = o.label || 'gate'
  const cmd = o.gate || o.scope.map((p) => 'go build ./' + p + '/... && go vet ./' + p + '/... && go test -count=1 ./' + p + '/...').join(' && ')
  // pipefail only matters with a pipe, and copilot's shell policy refuses the `set` prefix
  const full = '`' + (cmd.includes('|') ? 'set -o pipefail; ' : '') + cmd + '`' + (o.schemas ? '; if ' + stDiff(o.scope) + ' changes a function that writes a file and ./internal/schemas exists, also `go test -count=1 ./internal/schemas/`' : '')
  let pre = null
  // every result lands in ST_GATES (a run's exit reads the last gate per dir); fixer iterations are measured by numstat
  const out = async (status, exit, hist, residual) => {
    if (status !== 'green' && hist.length) log(tag + ' still ' + status + ' after ' + hist.length + ' iteration(s); residual: ' + residual.length)
    if (hist.length) stDelta('gate', (f) => o.scope.find((s) => f === s || f.startsWith(s + '/')) || stDir(f), pre, await stSnap(ctx, tag + ':after', o.scope))
    ST_GATES.push(...o.scope.map((dir) => ({ dir, exit: exit === 'green' ? 'green' : 'red', label: tag })))
    return { status, exit, gate: cmd, iterations: hist.length, filesChanged: [...new Set(hist.flatMap((h) => h.filesChanged))], residual, history: hist }
  }
  const check = (label) => stageAsk(ctx, o.check, label, 'Gate', ctx.rules + ' Run and report: ' + full + '. green=true only if every command exited 0. Edit nothing.', ST_CHECK)
  let first = ''
  if (o.check) {
    const c = await check(tag)
    if (c && c.green) return out('green', 'green', [], [])
    if (!o.max) return out('red', 'capped', [], [c ? c.output : stWhy(tag, 'gate check returned nothing')])
    first = c ? '\nInitial gate failure (before iteration 1):\n' + String(c.output).slice(-ST_TAIL) + '\n' : ''
  }
  if (o.max) pre = o.before || (await stSnap(ctx, tag + ':before', o.scope))
  const hist = []
  for (let i = 1; i <= (o.max || 0); i++) {
    const last = hist[hist.length - 1]
    const prior = last && last.residual.length ? '\nStill red after iteration ' + (i - 1) + ':\n- ' + last.residual.join('\n- ') + '\n' : ''
    const raw = (await stageAsk(ctx, o.fix, tag + ':' + i, 'Gate', 'Iteration ' + i + '/' + o.max + ' of a gate loop. ' + ctx.cd +
      'Method: ~/.claude/skills/problem-solving/references/verify.md (Gate Loop)\nGate: ' + full + '\nEdit ONLY under: ' + o.scope.join(', ') + '\nRules: ' + ctx.rules + '\n' + first + prior +
      '\nProtocol: run the whole gate (every command above, the conditional check included) and paste its tail; if every command exits 0 → status="green" and stop. Otherwise fix the FIRST failure per the Method, re-run the gate; repeat for at most ' + (o.fixes || 3) +
      ' distinct failures this iteration, then report (the rest go into residual verbatim). A failure outside scope goes into residual verbatim.' +
      ' If the gate cannot run at all (permission denied, missing command), set status="error", residual to the exact refusal, and stop.', ST_LOOP)) ||
      { status: 'error', failuresBefore: 0, failuresAfter: 0, filesChanged: [], residual: [stWhy(tag + ':' + i, 'agent returned no result')], summary: 'no result' }
    // a fixer's green is a claim: the check re-runs the whole gate (schemas sub-check included) before it counts
    const v = raw.status === 'green' && o.check ? await check(tag + ':confirm:' + i) : null
    const r = raw.status === 'green' && o.check && !(v && v.green)
      ? { ...raw, status: 'red', failuresAfter: Math.max(1, raw.failuresAfter), residual: [v ? String(v.output).slice(-ST_TAIL) : stWhy(tag + ':confirm:' + i, 'gate re-check returned nothing')] } : raw
    hist.push({ iteration: i, ...r })
    log(tag + ' ' + i + '/' + o.max + ': ' + r.status + ' (' + r.failuresBefore + ' → ' + r.failuresAfter + ') changed=' + r.filesChanged.length)
    if (r.status === 'green') return out('green', 'green', hist, r.residual)
    if (r.status === 'error') return out('error', 'error', hist, r.residual) // an unrunnable gate never improves by retrying
    // stalled only when the count held AND the same failures remain: fixing A while B surfaces is progress
    const same = (x, y) => [...x].sort().join('\n') === [...y].sort().join('\n')
    if (last && last.status === r.status && r.failuresBefore === r.failuresAfter && last.failuresAfter === r.failuresAfter && same(last.residual, r.residual)) {
      log('no progress across two iterations — escalating instead of burning the cap')
      return out(r.status, 'stalled', hist, r.residual)
    }
  }
  const end = hist[hist.length - 1]
  return out(end ? end.status : 'red', 'capped', hist, end ? end.residual : [])
}
// stageFix: one implementer over confirmed findings in one package, measured and held to ctx.lineBudget, then the Test stage.
async function stageFix(ctx, o) {
  const item = (f) => '- ' + stRel(ctx, f.file) + ':' + (f.line || 0) + ' (' + (f.symbol || '?') + ', ' + f.severity + (stCost(f) ? ', ' + f.kind : '') + '): ' + (f.symptom || f.title) +
    (f.scenario ? '\n  scenario: ' + f.scenario : '') + (f.evidence ? '\n  evidence: ' + f.evidence : '')
  const list = o.findings.map(item).join('\n')
  const own = o.owns && o.owns.length ? ' You own ONLY: ' + o.owns.join(', ') + '; list any other file you would need in leftUndone.'
    : ' Edit only non-test files under ' + o.pkg + ', except a *_test.go file a finding below cites.'
  const merge = ctx.kind === 'consolidate' ? ' These are consolidation findings (duplicates, dead code, legacy, stale or redundant tests), not bugs: merge and delete; behaviour, messages and exit codes stay identical.' : ''
  const seam = ctx.seams ? ' Never change an exported signature or edit outside ' + o.pkg + ': when a fix needs either, leave that part undone and list it in `seams` as {file, need}.' : ''
  const at = o.owns && o.owns.length ? o.owns : [o.pkg]
  const pre = await stSnap(ctx, 'pre:' + o.label, at)
  const impl = await stageAsk(ctx, ctx.S.fix, 'fix:' + o.label, 'Fix', ctx.rules + ' Fix these confirmed ' + (merge ? 'findings' : 'defects') + ' in ' + o.pkg + ' and nothing else.' + own + merge + seam +
    ' The evidence is verified: do not re-audit; first tool call: cat the cited files. The Test stage repairs tests; that is not unfinished work, so report done. ' +
    'Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 4-6). Root cause first, smallest correct change, functions < 50 lines, 1-line comments. ' +
    'Grep every caller outside the package of each function you changed and report in `callers` whether any now duplicates or contradicts the new behavior (do not edit them). ' +
    'Run the repo formatter and linter on what you touched' + (o.owns ? '' : ' (gofmt + go vet ./' + o.pkg + '/... for Go)') + '. Return `files`: every repo-relative file you changed.\n' + list, ST_FIXW, o.pin)
  const done = !!(impl && impl.status === 'done')
  for (const s of (impl && impl.seams) || []) ST_SEAMS.push({ pkg: o.pkg, file: stRel(ctx, s.file), need: s.need })
  const b = await stBudget(ctx, o, own + '\n' + list, done, pre, at)
  const tdirs = [...new Set([o.pkg, ...((impl && impl.files) || []).map((f) => stDir(stRel(ctx, f))).filter((d) => d === o.pkg || d.startsWith(o.pkg + '/'))])]
  // the owner's test rule: repair what the edits broke, write nothing new, propose the rest with a reason and a runtime
  const test = done && o.tests !== false ? await stageAsk(ctx, ctx.S.test, 'test:' + o.label, 'Test', ctx.rules + ' Fixes just landed in ' + o.pkg + ' (' + stDiff([o.pkg]) + '; changed: ' + (impl.files || []).join(', ') + '). ' +
    'Run the tests of ' + tdirs.join(', ') + ' and repair only the tests these fixes broke. Edit only *_test.go and testdata files directly in ' + tdirs.join(', ') + '; never another package\'s tests or shared fixtures (list any you would need in leftUndone). ' +
    'Write NO new test.' + ST_PROPOSE + ' Put "n/a" in callers.\nThe fixes:\n' + list, ST_TESTW) : null
  for (const p of (test && test.proposed) || []) ST_PROPOSED.push({ pkg: o.pkg, ...p })
  let snap = b.snap
  if (test) { const s = await stSnap(ctx, 'test:' + o.label, at); stDelta('test', o.pkg, snap, s); snap = s }
  const files = [...new Set([...((impl && impl.files) || []), ...((b.shrink && b.shrink.files) || []), ...((test && test.files) || [])].map((f) => stRel(ctx, f)))]
  return { pkg: o.pkg, impl, test, done, testOk: o.tests === false || !!(test && test.status === 'done'), files, over: b.over, snap }
}
// stBudget measures the fix; over ctx.lineBudget it gets ONE shrink-or-justify iteration, still over lands in ST_OVER (never reverted).
async function stBudget(ctx, o, task, done, pre, at) {
  let snap = await stSnap(ctx, 'fix:' + o.label, at)
  const fr = stDelta('fix', o.pkg, pre, snap)
  let net = fr.prod.net
  let shrink = null
  if (done && fr.measured && stOver(ctx, net)) {
    shrink = await stageAsk(ctx, ctx.S.fix, 'shrink:' + o.label, 'Fix', ctx.rules + ' Line budget ' + ctx.lineBudget + ': production net must be ≤ ' + ST_BUDGET[ctx.lineBudget] + ' lines in ' + o.pkg +
      '; your fix measured ' + net + ' (git diff --numstat). Shrink it or justify it: cut what the fix does not need (reuse an existing helper, drop a new abstraction, delete what it made dead) and keep the fix.' +
      ' Never revert the fix, never git checkout/restore/stash. If it cannot shrink, edit nothing and put the reason in summary.' + task, ST_WORK)
    const s = await stSnap(ctx, 'shrink:' + o.label, at)
    net += stDelta('shrink', o.pkg, snap, s).prod.net
    snap = s
  }
  const over = done && fr.measured && stOver(ctx, net) ? { pkg: o.pkg, net, budget: ctx.lineBudget, why: (shrink && shrink.summary) || '' } : null
  if (over) ST_OVER.push(over)
  return { snap, shrink, over }
}
// stageRepair: stageFix, then stageGate (ctx.gate, else a Go gate) once the fix is done; green is the gate's, never a stage's claim.
async function stageRepair(ctx, pkg, findings, o) {
  const fx = await stageFix(ctx, { pkg, findings, label: o.label })
  const gate = fx.done ? await stageGate(ctx, { scope: [o.scope || pkg], check: ctx.S.gate, fix: ctx.S.fix, max: ctx.gateMax || 0, schemas: true, label: o.gate, gate: ctx.gate, before: fx.snap }) : null
  return { ...fx, gate, green: fx.done && !!gate && gate.exit === 'green', files: [...fx.files, ...(gate ? gate.filesChanged : [])] }
}
// stagePolish: once over a merged diff: Dedupe (2+ packages), Simplify, Review, then one stageRepair per package.
async function stagePolish(ctx, o) {
  const files = [...new Set(o.files.map((f) => stRel(ctx, f)))]
  const pkgs = [...new Set(files.map(stDir))]
  const scope = ' Only these files (the tree is shared; never touch or diff others): ' + files.join(' ') + '. Diff: ' + stDiff(files) + '.'
  const cut = ' Name the assumption before every cut; flag instead of cutting when it has no covering test; never cut semantic comments, error context or distinct tests; never fix bugs (flag them). ' +
    'After edits run gofmt, go vet and go test for ' + pkgs.join(', ') + '; if a test regresses, revert your own edit by hand (never git checkout/restore/stash) and flag it. ' +
    'Your net lines (measured by git diff --numstat afterwards) must be ≤ 0: remove more than you add, or apply nothing. ' +
    'Return applied[] and flagged[] as `file:line · what · assumption`, deltaLines (net changed lines, negative = removed) and status (clean = nothing to do).'
  // Dedupe and Simplify are held to a measured net ≤ 0 whatever the budget; over it they report partial with the number
  let snap = await stSnap(ctx, 'polish', pkgs)
  const held = async (k, r) => {
    const s = await stSnap(ctx, k, pkgs)
    const d = stDelta(k, stDir, snap, s)
    snap = s
    const net = d.prod.net + d.test.net
    const out = r && d.measured && net > 0 ? { ...r, status: 'partial', measured: net } : r ? { ...r, measured: d.measured ? net : null } : r
    lines.push(k + ': ' + (out ? out.applied.length + ' applied, ' + out.flagged.length + ' flagged, measured Δ ' + (d.measured ? net : '?') + ' lines (' + out.status + (net > 0 ? ': net must be ≤ 0' : '') + ')' : 'agent returned nothing'))
    return out
  }
  const lines = []
  let dedupe = null
  if (pkgs.length >= 2 && ctx.S.dedupe) {
    phase('Dedupe')
    dedupe = await held('dedupe', await stageAsk(ctx, ctx.S.dedupe, 'dedupe', 'Dedupe', ctx.rules + ' Cross-package dedupe, the reuse angle of ' + ctx.cwd + '/skills/simplify/SKILL.md.' + scope +
      ' Look ONLY for helpers or logic duplicated across these packages (grep siblings, `git log -S`); compare a near-duplicate with its original for behavior the copy lost, then consolidate at the original (callers import it; no new shim).' + cut, ST_SIMP))
  } else lines.push('dedupe skipped: ' + pkgs.length + ' package(s)')
  phase('Simplify')
  const simplify = await held('simplify', await stageAsk(ctx, ctx.S.simplify, 'simplify', 'Simplify', ctx.rules + ' Simplify gate (' + ctx.cwd + '/skills/simplify/SKILL.md, quality.md -> Simplify), run ONCE over the merged diff.' + scope +
    ' Run `claude-code review simplify ' + files.join(' ') + '`, then one in-context pass (reuse, simplification, efficiency, altitude). Edit only the listed non-test files.' + cut, ST_SIMP))
  phase('Review')
  const rv = await stageAsk(ctx, ctx.S.review, 'review', 'Review', ctx.rules + ' Read-only: edit nothing, never run go test. Review the final diff with the ' + ctx.cwd +
    '/skills/evaluate-code/SKILL.md correctness and contract lenses (wrong results, broken invariants, changed behavior callers rely on, errors dropped).' + scope +
    ' Each finding: file, line, symbol, one-line symptom, severity. Return {findings: []} when clean.', ST_REVF)
  const findings = rv ? rv.findings.map((f) => ({ ...f, file: stRel(ctx, f.file) })) : []
  lines.push('review: ' + (rv ? findings.length + ' finding(s)' : 'agent returned nothing'))
  const byPkg = {}
  for (const f of findings) if (pkgs.includes(stDir(f.file))) (byPkg[stDir(f.file)] = byPkg[stDir(f.file)] || []).push(f)
  // a finding outside the diff's packages has no Fix-all owner: unresolved, and MEDIUM+ keeps the polish red
  const incomplete = findings.filter((f) => !pkgs.includes(stDir(f.file)))
  const open = incomplete.filter((f) => f.severity !== 'low')
  if (incomplete.length) lines.push('unresolved: ' + incomplete.length + ' review finding(s) outside the fixed packages (' + open.length + ' MEDIUM+): ' + incomplete.map((f) => f.file + ':' + f.line).join(', '))
  // a fixer edits under its package, nested ones too: run in waves by ancestor count, so ancestors finish first
  const keys = Object.keys(byPkg)
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
  const fixall = keys.map((p) => byKey[p]).filter(Boolean)
  for (const x of fixall) lines.push('fix-all ' + x.pkg + ': ' + (x.done ? x.findings + ' fixed' : 'NOT done') + ', gate ' + (x.green ? 'green' : 'red'))
  // a Dedupe/Simplify edit no Fix-all re-gated gets a gate of its own (unmeasured edits: every package); its green is never assumed
  const unknown = [dedupe, simplify].some((r) => r && r.measured === null && r.applied.length)
  const edited = (unknown ? pkgs : [...new Set(ST_LINES.filter((r) => r.measured && /^(dedupe|simplify)$/.test(r.stage)).map((r) => r.pkg))]).filter((p) => !keys.includes(p))
  const pg = edited.length ? await stageGate(ctx, { scope: edited, check: ctx.S.gate, fix: ctx.S.fix, max: ctx.gateMax || 0, schemas: true, label: 'polish-gate', gate: ctx.gate }) : null
  if (pg) lines.push('polish gate ' + edited.join(', ') + ': ' + pg.exit)
  const report = ['', '## Polish', ...lines.map((l) => '- ' + l), ...findings.map((f) => '- **' + f.severity + '** `' + f.file + ':' + f.line + '` (' + f.symbol + ') — ' + f.symptom)].join('\n')
  // a review that never ran proves nothing, and a MEDIUM+ finding with no owner stays open: both hold red whatever a gate says
  const hold = [...new Set([...open.map((f) => stDir(f.file)), ...(rv ? [] : pkgs)])]
  const red = [...new Set([...fixall.filter((x) => !x.green).map((x) => x.pkg), ...(pg && pg.exit !== 'green' ? edited : []), ...hold])]
  return { dedupe, simplify, review: { findings, ran: !!rv }, fixall, incomplete, green: !red.length, red, hold, files: [...new Set([...fixall.flatMap((x) => x.files), ...(pg ? pg.filesChanged : [])])], decisions: lines, report }
}
// ── end stages ──

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
    gate: stepOf(a.steps, 'gate', { model: 'sonnet', effort: 'medium', tools: 'edit' }, OPT),
    depscan: stepOf(a.steps, 'depscan', { model: 'sonnet', effort: 'medium' }, OPT),
  }
} catch (e) { return ERR(e.message) }
for (const k of ['profile', 'provider']) if (a[k] !== undefined && !/^[A-Za-z0-9._-]+$/.test(String(a[k]))) return ERR('args.' + k + ' must be a plain name')
let PF_STATE = '' // claude account state, read by the coworkers preflight when it runs
const EXT = [...new Map(Object.values(S).filter((s) => s.ext).map((s) => [s.agent + '|' + s.profile + '|' + s.provider, s])).values()]
if (EXT.length) {
  const pf = await ag(
    'For each line, run it and report its candidate count and the first candidate as agent/account/model. Edit nothing. Also run `claude-code gateway --format json --agent claude --fallback none --model sonnet </dev/null` and report claudeState = candidates[0].state when candidates is non-empty, else the skipped[] entry for claude and its gate value, else "unknown".\n' +
      EXT.map((s) => `claude-code gateway --agent ${s.agent} --fallback agents${s.profile ? ' --profile ' + s.profile : ''}${s.provider ? ' --provider ' + s.provider : ''} --format json </dev/null`).join('\n'),
    { label: 'coworkers-preflight', phase: 'Preflight', model: 'haiku', effort: 'low', schema: { type: 'object', additionalProperties: false, required: ['routes'], properties: { claudeState: { type: 'string' }, routes: { type: 'array', items: { type: 'object', additionalProperties: false, required: ['agent', 'candidates', 'first'], properties: { agent: { type: 'string' }, candidates: { type: 'integer' }, first: { type: 'string' }, error: { type: 'string' } } } } } } },
  )
  PF_STATE = (pf && pf.claudeState) || ''
  for (const r of (pf && pf.routes) || []) log(`coworkers preflight ${r.agent}: ${r.candidates} route(s), first ${r.first}${r.error ? ' — ' + r.error : ''}`)
  const routes = (pf && pf.routes) || []
  // fail closed: a missing preflight, or no route row for an external agent, counts as no route
  const dead = [...routes.filter((r) => r.candidates === 0).map((r) => r.agent + (r.error ? ' (' + r.error + ')' : '')),
    ...[...new Set(EXT.map((s) => s.agent))].filter((g) => !routes.some((r) => String(r.agent).includes(g))).map((g) => g + ' (no preflight result)')]
  if (dead.length) return ERR('coworkers: no route for ' + dead.join(', ') + '; BLOCKED before any work')
}
// Audit effort stays per lens unless steps.audit.effort is set; fix keeps opus+high for highs unless set.
const AUDIT_EFFORT = RAW.audit && RAW.audit.effort
const FIX_MODEL = RAW.fix && !S.fix.ext && RAW.fix.model
const FIX_EFFORT = RAW.fix && RAW.fix.effort

// ── Pressure: Claude account state read once; low/exhausted steps every Claude stage down one model ──
const routeStateP = PF_STATE ? Promise.resolve({ state: PF_STATE }) : ag(
  `Run exactly: ${CD}claude-code gateway --format json --agent claude --fallback none --model sonnet. ` +
    `It prints one JSON object; state = candidates[0].state when candidates is non-empty, else the skipped[] entry whose agent ` +
    `is "claude" and its gate value, else "unknown". Never run anything else.`,
  { label: 'route-state', phase: 'Scope', schema: { type: 'object', additionalProperties: false, required: ['state'], properties: { state: { type: 'string' } } }, model: 'haiku', effort: 'low' }
).catch(() => null)
const MODEL_DOWN = { opus: 'sonnet', sonnet: 'haiku', haiku: 'haiku' }

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
// kind consolidate: the lenses look for merge-and-delete candidates, never bugs; behaviour, messages and exit codes stay identical
const CONSOLIDATE_DIMENSIONS = [
  { key: 'duplicates', prompt: 'consolidation candidates, not bugs: duplicated helpers or logic across files or packages, legacy paths and shims no caller needs (verify with grep)' },
  { key: 'dead', prompt: 'consolidation candidates, not bugs: dead code with no caller (verify with grep), stale or redundant tests that pin nothing the other tests miss', effort: 'low' },
]
let FILES_MODE = !RANGE && PATHS.length > 0
// A caller lens may be a bare prompt string or lack a key; both get key lens<i> (labels read find:1:undefined:7).
const asDims = ds => ds.map((d, i) => typeof d === 'string' ? { key: 'lens' + i, prompt: d } : { ...d, key: d.key || 'lens' + i })
const ARG_DIMS = Array.isArray(a.dimensions) && a.dimensions.length ? asDims(a.dimensions) : null
let DIMENSIONS = ARG_DIMS || (JOB === 'consolidate' ? CONSOLIDATE_DIMENSIONS : FILES_MODE ? CODE_DIMENSIONS : DEFAULT_DIMENSIONS)
const FOCUS = a.focus ? ` Caller focus: ${a.focus}.` : ''
// Group packages/files ≤4 per auditor so one context holds the whole group.
let GROUPS = []
for (let i = 0; i < Math.max(1, PKGS.length); i += 4) GROUPS.push(PKGS.slice(i, i + 4))
// Files mode: a whole tree (internal/: 1378 files, 112 KB) overflowed a scope leaf's Bash output and came back empty.
const SCOPE_MAX = 200 // files per scope leaf, ~10 KB of compact JSON
let SPLIT = false // GROUPS hold exact package dirs; the scope command filters the PATHS listing to them
const SPLIT_SCHEMA = { type: 'object', additionalProperties: false, required: ['dirs'], properties: { dirs: { type: 'array', items: {
  type: 'object', additionalProperties: false, required: ['dir', 'files'], properties: { dir: { type: 'string' }, files: { type: 'integer' } } } } } }
let splitP = null
if (FILES_MODE && !PRE && !(Array.isArray(a.packages) && a.packages.length)) {
  splitP = ag(
    `Run exactly: ${CD}claude-code review scope --repo ${CWD || '.'} --paths ${PATHS.join(',')} --format json </dev/null | ` +
      `jq -c '[.files[].path | sub("/[^/]*$"; "")] | group_by(.) | map({dir: .[0], files: length})' — and return its array verbatim as dirs. Never run anything else.`,
    { label: 'scope-split', phase: 'Scope', schema: SPLIT_SCHEMA, model: 'haiku', effort: 'low' }
  ).catch(() => null)
}
const scopeCmd = g => SPLIT
  ? `claude-code review scope --repo ${CWD || '.'} --paths ${PATHS.join(',')} --format json </dev/null | jq -c --argjson d '${JSON.stringify(g)}' '{files: [.files[] | select(.path | sub("/[^/]*$"; "") | IN($d[]))]}'`
  : `claude-code review scope --repo ${CWD || '.'} ${FILES_MODE ? '' : '--range ' + (RANGE || 'HEAD~1..HEAD') + ' '}--paths ${g.join(',') || '.'} --format json </dev/null`

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
  symptom: f.symptom || f.title,
  severity: ORDER[f.severity] === undefined ? (log(`unknown severity '${f.severity}' on ${f.file} → medium`), 'medium') : f.severity,
})
const same = (g, f) => sameBug({ cwd: CWD }, g, f)
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
// Setup runs concurrently: route state, the scope split and round 1's unsplit scope (kept unless the split regroups).
const runScopes = (round, model) => parallel(GROUPS.map((g, gi) => () =>
  ag(S.scope.task(
    `${CD}run ONE command: ${scopeCmd(g)} — and return its files array verbatim ({path, bytes}). Do not read the files; the auditors cat their own chunk. At most 2 tool calls.`),
    { label: `scope:${round}:${gi}`, phase: 'Scope', schema: S.scope.schema(SCOPE_SCHEMA), ...S.scope.type, model: model, effort: S.scope.effort } // file listing: schema-checked, haiku is enough
  ).then(r => S.scope.done(`scope:${round}:${gi}`, r))
))
const SCOPE_FIXED = (MODEL_DOWN[S.scope.model] || S.scope.model) === S.scope.model // pressure cannot change its model
let earlyScopes = !PRE && SCOPE_FIXED ? runScopes(1, S.scope.model) : null
const split = splitP && await splitP
const dirs = split && split.dirs || []
if (dirs.reduce((n, d) => n + d.files, 0) > SCOPE_MAX) {
  GROUPS = []
  let cur = [], n = 0
  for (const d of dirs) {
    if (cur.length && n + d.files > SCOPE_MAX) { GROUPS.push(cur); cur = []; n = 0 }
    cur.push(d.dir); n += d.files
  }
  if (cur.length) GROUPS.push(cur)
  SPLIT = true
  earlyScopes = null // the unsplit listing is discarded; the loop scopes the split groups
  log(`scope split: ${dirs.length} dir(s) → ${GROUPS.length} scope group(s) of ≤${SCOPE_MAX} files`)
}
const routeState = await routeStateP
const PRESSURE = !!(routeState && (routeState.state === 'low' || routeState.state === 'exhausted'))
const routeModel = m => (PRESSURE && MODEL_DOWN[m]) ? MODEL_DOWN[m] : m
if (PRESSURE) log(`pressure: claude account ${routeState.state} — stepping Claude stages down one model`)
else if (!routeState) log('pressure: route read failed — no downshift (fail open)')
const EXT_CAP = Math.min(CAP, 4)
const limiter = n => { let busy = 0; const q = []; const next = () => { if (busy < n && q.length) { busy++; q.shift()() } }
  return fn => new Promise((res, rej) => { q.push(() => fn().then(res, rej).finally(() => { busy--; next() })); next() }) }
// Fix/gate stage context: bare commands (no `cd … &&`; copilot rejects compound commands), external agents use --workdir.
const FCTX = { cwd: CWD, cd: CWD ? `cwd: ${CWD} (external agents: ask --workdir ${CWD}). Prefix every shell command with \`cd ${CWD} &&\` (each Bash call starts in the session dir; the copilot shell allows a cd inside its roots).\n` : '', rules: SHARED_TREE, S, model: m => { if (++AGENTS > MAX_AGENTS) LIMIT_HIT = true; return routeModel(m) }, // counts stage-block agents
   kind: JOB, lineBudget: LINE_BUDGET }
const GATE_RULES = 'Never run git stash|checkout|reset|restore|clean|commit; never run install/deploy/release targets or edit tool config. Edit only inside scope; re-run only the gate.'
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
  const scopes = PRE ? [] : (round === 1 && earlyScopes) ? await earlyScopes : await runScopes(round, routeModel(S.scope.model))
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
  const short = !PRE && SPLIT ? GROUPS.map((g, gi) => ({ g, gi, n: dirs.filter(d => g.includes(d.dir)).reduce((t, d) => t + d.files, 0) })).find(x => scopes[x.gi] && !scopes[x.gi].files.length && x.n > 0) : null
  if (short) {
    rounds.push({ error: `code-analyzer: scope:${round}:${short.gi} returned 0 files for ${short.g.join(', ')} (split counted ${short.n}) — listing likely truncated; rerun with narrower paths`, exit: 'error', round })
    break
  }
  if (!PRE && FILES_MODE && !chunks.length) {
    rounds.push({ error: 'code-analyzer: no source files under ' + PATHS.join(', '), exit: 'error', round })
    break
  }
  if (!PRE && !FILES_MODE && !chunks.length) {
    rounds.push({ error: `code-analyzer: ${RANGE} changes no source files under ${PATHS.join(', ') || '.'}`, exit: 'error', round })
    break
  }
  const planned = DIMENSIONS.filter(d => !d.hitsOnly).length * chunks.length
  if (AGENTS + planned > MAX_AGENTS) {
    rounds.push({ error: `code-analyzer: ${planned} first-pass auditors planned (${AGENTS} agents already run) exceed the agent limit ${MAX_AGENTS}; narrow paths, raise chunkChars, or raise maxAgents`, exit: 'error', round })
    break
  }
  log(`${chunks.length} chunk(s) of ≤${CHUNK_CHARS} chars → ${planned} first-pass auditors`)

  // ── Find → Refute as ONE pipeline: a chunk's findings are refuted while other chunks still audit ──
  // hitsOnly lenses (contracts) run in a second pass over just the files the first pass confirmed a finding in:
  // round 9–10 measured contracts at a third of Find while yielding mostly lows.
  const mkUnits = (dims, cs, pass) => dims.flatMap(d => cs.map((c, ci) => ({ dim: d, group: c.group, key: `${d.key}:${pass}${ci}`, chunk: c })))
  const primaryDims = DIMENSIONS.filter(d => !d.hitsOnly)
  const hitDims = DIMENSIONS.filter(d => d.hitsOnly)
  log('auditing ' + TARGET + ' — ' + primaryDims.length + ' lens(es) × ' + chunks.length + ' chunk(s)' + (hitDims.length ? ', then ' + hitDims.map(d => d.key).join('/') + ' over flagged files' : ''))
  const seen = [] // proximity-merged across this round's chunks and lenses; a PRIOR match still flows on to the stalled check
  let mergedCount = 0
  const refuted = []
  const unverified = []
  let refuteCalls = 0
  let auditorCount = 0
  const findStage = u => {
    auditorCount++
    const prompt =
      `Read-only audit of ${TARGET}${IN}. Packages: ${u.group.join(', ') || 'all touched'}. Lens: ${u.dim.prompt}.${FOCUS}\n` +
      (FILES_MODE
        ? `First tool call: cat the files listed below (one call, all of them). Judge from that; open another file only to check a caller or a test (budget 8 tool calls). `
        : `First tool call: ${CD}git diff ${RANGE || 'HEAD~1..HEAD'} -- <the files listed below> (one call, all of them). Judge from that diff; open a file only to check a caller or a test named in it (budget 8 tool calls). `) +
      `Report only defects you can evidence with a grep, diff or test run — no style nits. ${SHARED_TREE}\n` +
      `Method: ~/.claude/skills/evaluate-code/SKILL.md (report only findings on changed code you verified; >80% confidence filter)\n\n` +
      (u.chunk.files.length
        ? `Files: ${u.chunk.files.map(f => f.path).join(' ')}\n\n`
        : `(the scope agent returned nothing — ${FILES_MODE ? 'read the files under ' + u.group.join(' ') : 'run git diff --stat ' + (RANGE || 'HEAD~1..HEAD') + ' -- ' + (u.group.join(' ') || '.')} yourself)`)
    return auditGate(() => ag(S.audit.task(prompt),
      { label: `find:${round}:${u.key}`, phase: 'Find', schema: S.audit.schema(FINDINGS_SCHEMA), ...S.audit.type, model: routeModel((RAW.audit && RAW.audit.model) || u.dim.model || S.audit.model), effort: AUDIT_EFFORT || u.dim.effort || 'low' }
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
      // A range ending before HEAD: refuters once judged the working tree and rejected true findings about the range.
      const end = (RANGE || 'HEAD~1..HEAD').split(/\.{2,3}/).pop() || 'HEAD'
      const at = FILES_MODE ? '' : `These findings describe the change ${RANGE || 'HEAD~1..HEAD'}: judge the code as of its end commit ${end} (git show ${end}:<path>, git grep <pattern> ${end}) wherever the working tree differs from it; later commits do not refute a finding. `
      const brief = `Try to REFUTE each of these ${fs.length} audit finding(s) in ${pkg} against the real tree${IN} (read-only); one verdict per index.\n${list}\n\n` + at +
        `Refuted means: the code path does not do what is claimed, the scenario cannot occur, or it is already handled; quote the line that decides it. ` +
        `Default to refuted=true when the evidence does not hold up. A TRUE claim with a WRONG file or line is NOT refuted: set refuted=false and give the correct file and line. ` +
        `That a change was deliberate, already merged or allowed by a repo rule does NOT refute a finding: only facts about the code do. ` +
        // A "no such verb" finding once survived and its fixer rewrote a correct doc; the verb was an alias spelling.
        `A claim that a verb, flag, symbol or file does NOT exist is refuted unless a tree-wide grep (alias, registration and config tables included) AND the repo's own test or CLI output both come back empty.`
      const label = `refute:${round}:${pkg.split('/').pop()}:${bi}`
      return refuteGate(() => ag(S.refute.task(brief), { label, phase: 'Refute', schema: S.refute.schema(VERDICTS_SCHEMA), ...S.refute.type, model: routeModel(S.refute.model), effort: S.refute.effort })).then(v => {
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
  if (LIMIT_HIT) { rounds.push({ error: LIMIT_MSG, exit: 'error', round, confirmed, refuted, unverified }); break }
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
  // a finding the refuter never judged is unchecked, not clean: incomplete, never dry
  if ((!FIX && !PART) || !toFix.length) { rounds.push({ ...base, fixed: [], gate: null, exit: !toFix.length && unverified.length ? 'incomplete' : FIX ? 'dry' : 'no-fix', metrics }); break }
  // Same findings as an earlier round (PRIOR, accumulated across rounds): report, do not re-fix forever.
  if (PRIOR.length && toFix.every(f => PRIOR.some(g => same(g, f)))) { rounds.push({ ...base, fixed: [], gate: null, exit: 'stalled', metrics }); break }

  // ── Scan → Plan: dependency scan per package, then a deterministic wave partition ──
  const byPkg = new Map()
  for (const f of toFix) byPkg.set(f.package, [...(byPkg.get(f.package) || []), f])
  phase('Scan')
  const scans = await parallel([...byPkg.entries()].map(([pkg, fs]) => () => ag(S.depscan.task(
    `Read-only dependency scan of ${pkg}${IN}. A fixer will repair these findings:\n${fs.map(f => `- ${relPath(f.file)}:${f.line} ${f.title}`).join('\n')}\n` +
      `Return files = every file the fix will likely edit (tests included), exported = whether it changes an exported symbol or signature, ` +
      `importers = repo packages importing ${pkg} (run once: ${CD}claude-code review scope --repo ${CWD || '.'} --paths ${pkg} --importers --format json </dev/null and take packages[].importers), ` +
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
  const fixTask = async (pkg, fs, owns, hotspot) => {
    const high = fs.some(f => ORDER[f.severity] <= ORDER.high)
    const tests = !hotspot && TESTS && fs.some(f => !f.noTest && SRC_RE.test(relPath(f.file))) // docs-only or all-noTest: no test agent
    const fx = await stageFix(FCTX, { pkg, findings: fs, owns, tests, label: round + ':' + pkg, pin: { model: FIX_MODEL || (high ? 'opus' : 'sonnet'), effort: FIX_EFFORT || (high ? 'high' : 'medium') } })
    // a test stage that did not finish (blocked or partial) blocks the task like an unfinished fix
    return { package: pkg, pkg, findings: fs.length, impl: fx.impl, tests: fx.test, test: fx.test, blocked: !fx.done || !fx.testOk, notDone: !fx.done, testNotDone: fx.done && !fx.testOk }
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

  // ── Gate (ADK LoopAgent, in-script stageGate; a.gate overrides, else stageDetect picks one) ──
  const pkgs = [...byPkg.keys()]
  const det = a.gate ? { gate: a.gate, source: 'args' } : await stageDetect(FCTX, S.scope, pkgs)
  phase('Gate')
  const gate = det.gate
    ? { ...(await stageGate({ ...FCTX, rules: GATE_RULES }, { scope: pkgs, gate: det.gate, check: S.scope, fix: S.gate, max: MAX_ITER, label: 'gate:' + round })), gateSource: det.source }
    : { status: 'error', exit: 'error', gate: null, gateSource: det.source, iterations: 0, filesChanged: [], residual: ['no gate detected under ' + (CWD || 'cwd') + '; pass args.gate'], history: [] }

  const metricsOut = { ...metrics, fixPairs: byPkg.size, gateIterations: gate ? gate.iterations : 0 }
  const out = { ...base, fixed, gate, partition, plan: planMd, metrics: metricsOut }
  if (!gate || gate.exit !== 'green' || out.fixed.some(x => x.notDone)) { rounds.push({ ...out, exit: 'blocked' }); break }
  const touched = [...new Set([
    ...out.fixed.flatMap(x => [...((x.impl && x.impl.files) || []), ...((x.tests && x.tests.files) || [])]),
    ...(gate.filesChanged || []),
  ].map(relPath))].filter(p => SRC_RE.test(p) && !TEST_RE.test(p))
  if (!touched.length) { rounds.push({ ...out, exit: 'nothing-changed' }); break }
  if (round >= ROUNDS) { rounds.push({ ...out, exit: 'capped' }); break }
  if (lowBudget()) {
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
  SPLIT = false
  GROUPS = []
  for (let i = 0; i < Math.max(1, PKGS.length); i += 4) GROUPS.push(PKGS.slice(i, i + 4))
  DIMENSIONS = ARG_DIMS || (JOB === 'consolidate' ? CONSOLIDATE_DIMENSIONS : CODE_DIMENSIONS)
  TARGET = 'the code under ' + PATHS.join(', ')
  PRE = null
}

const lastRound = rounds[rounds.length - 1] || { exit: 'error', round: 0 }
const allConfirmed = rounds.flatMap(r => r.confirmed || [])
const unverifiedAll = rounds.flatMap(r => r.unverified || [])
const fixedAll = rounds.flatMap(r => r.fixed || [])
// Red: a package whose fixer is not done, or whose LAST gate (a later round's re-gate wins) is not green.
const notDone = rounds.flatMap(r => (r.fixed || []).filter(x => x.notDone).map(x => x.package))
const red = [...new Set([...stGateRed([...ST_GATES.map(g => g.dir), ...rounds.flatMap(r => (r.fixed || []).filter(x => x.package !== 'hotspots').map(x => x.package))]), ...notDone])].sort()
const lines = stLines(ST_LINES)
if (ST_OVER.length) decisions.push(`over line budget ${LINE_BUDGET}: ${ST_OVER.map(o => o.pkg + ' net ' + o.net).join(', ')}`)
for (const r of rounds) {
  for (const x of r.fixed || []) decisions.push(`${x.notDone ? 'fix NOT done (' + (x.impl ? x.impl.status : 'no result') + ') for ' : 'fixed '}${x.findings || 0} finding(s) in ${x.package}` +
    (x.testNotDone ? ` — test stage ${x.test ? x.test.status : 'returned nothing'}` : ''))
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
  ...(planOut ? ['', planOut] : [])].join('\n') + (ST_LINES.length || ST_PROPOSED.length ? '\n' + stTail(lines, ST_OVER, ST_PROPOSED) : '')
// a fix still over the line budget after its one shrink pass turns a finished run into review (failures keep their exit)
const EXIT = LIMIT_HIT ? 'error' : STARVED && lastRound.exit !== 'error' ? 'blocked' : ST_OVER.length && ['capped', 'nothing-changed', 'dry'].includes(lastRound.exit) ? 'review' : lastRound.exit
return {
  ...lastRound, exit: EXIT, ...(LIMIT_HIT ? { error: LIMIT_MSG } : {}), round: lastRound.round, rounds,
  route: 'code-analyzer', packages: PKG_COUNT, merged: totalMerged, confirmed: allConfirmed, // every round's, not only the last re-audit's
  dropped: rounds.flatMap(r => (r.refuted || []).map(f => ({ file: relPath(f.file), line: f.line, symptom: f.title }))),
  unverified: unverifiedAll, fixed: fixedAll, red, decisions, report, partition: partitionOut, coworkers: COWORKERS,
  lines, proposedTests: ST_PROPOSED, over: ST_OVER, gates: ST_GATES,
}
