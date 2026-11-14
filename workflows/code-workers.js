export const meta = {
  name: 'code-workers',
  description: 'Streaming relay: each package flows find → rederive → fix → test repair → gate (fix-on-red ≤ maxIterations) on its own, no barrier, every stage numstat-measured; then one polish over the merged diff',
  whenToUse:
    'USE: audit and repair several Go packages (or a LOW/MEDIUM job\'s files) without a barrier between them; review-only with fix:false. CALL: Workflow({scriptPath: "~/.claude/workflows/code-workers.js", args: {cwd, packages: [...] | paths: [...]}}). OPTIONAL: slice (run only the first N packages), fix (default true), minFix (default low: every finding is fixed, decision #54), focus, known [{file, symptom, severity?}] (bug already named: skips Find and Rederive for that file\'s package), fast (false ignores known), kind (fix|feature|refactor|audit|consolidate; consolidate swaps the find lenses for duplicates, dead code, legacy and stale tests and merges or deletes them, behaviour unchanged), lineBudget (shrink|neutral|free; default by kind: refactor neutral, consolidate shrink, else free; an over-budget fix gets one shrink-or-justify pass, still over exits review), maxIterations (gate fix-on-red iterations per package, default 2; 0 = check only), gate (one shell command used as every package\'s gate, e.g. npm test for JS/TS; default go build/vet/test ./<pkg>/...), polish (default true: one Dedupe, Simplify, Review and Fix-all over the merged diff after the stream; false skips it, as under code-engineer, and also forbids exported-signature or cross-package edits: they come back in seams; also skipped under 150k budget tokens left), steps {find, rederive, fix, test, gate, dedupe, simplify, review} = {agent?, model?, effort?, profile?, provider?} per stage (agent claude|codex|copilot|gemini|opencode; external stages run on their pins, or args/steps profile|provider, and fall back on 429/auth; each reports ran on or BLOCKED; fix-all and gate fix iterations use steps.fix). OPTIONAL: profile, provider (run-wide override for external stages), profiles (default [dev, qa, prod]: under provider databricks with no profile, packages take them round-robin). An unknown arg is refused at launch. Test stages repair what the fixes broke and write nothing new; every other test comes back in proposedTests. RETURNS {exit (green only when every touched package\'s last gate is green), route, packages, merged, confirmed, dropped, unverified, fixed, red, polish, files, foreign, seams, lines (numstat per stage, per package and total, production vs test), proposedTests, over, gates, decisions, report, coworkers}. Details: saved-workflows.md.',
  phases: [
    { title: 'Find', detail: 'two lenses per package; claims merged by file ±10 lines or same symbol within the package' },
    { title: 'Rederive', detail: 'each claim re-derived blind from file:line + symptom' },
    { title: 'Fix', detail: 'as soon as a package has confirmed findings at or above minFix, one implementer fixes them' },
    { title: 'Test', detail: 'repair tests the fix broke; every new test is proposed (proposedTests), never written' },
    { title: 'Gate', detail: 'package-scoped build/vet/test, plus internal/schemas when a writer changed; on red up to maxIterations fixer iterations' },
    { title: 'Dedupe', detail: 'once over the merged diff after the stream, when 2+ packages were fixed' },
    { title: 'Simplify', detail: 'once over the merged diff after the stream; Dedupe first when 2+ packages were fixed (polish:false skips)' },
    { title: 'Review', detail: 'once over the merged diff after the stream; each package with findings earns one Fix-all, Test and re-Gate' },
  ],
}

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
  const profile = v.profile !== undefined ? v.profile : o.profile || ''
  const provider = v.provider !== undefined ? v.provider : o.provider || ''
  if (who === 'claude' && !profile && !provider) {
    const model = v.model !== undefined ? v.model : d.model || 'sonnet'
    const same = (_label, r) => r
    return { agent: 'claude', ext: false, model, effort, type: d.agentType ? { agentType: d.agentType } : {}, task: (p) => p, schema: (s) => s, done: same, text: same }
  }
  if (!o.external) throw new Error(`${where}.agent: ${who} is not wired in this workflow yet (claude only)`)
  // no profile/provider: Go routes the agent's pin; a named one leads, ladder after.
  const edit = d.tools === 'edit'
  const mode = edit || d.tools === 'run' ? '--tools' : who === 'copilot' ? '--read-only' : '--tools'
  const ask = `claude-code agents ask --agent ${who} --fallback agents --format json ${mode} --effort ${effort}` + (profile ? ` --profile ${profile}` : '') +
    (provider ? ` --provider ${provider}` : '') + (v.model ? ` --model ${v.model}` : '')
  const F = { ran_on: { type: 'string' }, hops: { type: 'integer' }, coworker: { type: 'string', enum: ['ok', 'BLOCKED'] }, coworker_error: { type: 'string' } }
  const R = ['ran_on', 'coworker']
  const record = (label, r) => {
    const { ranOn, canon, ok } = ranOnVerdict(r)
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
// ── stages: copied verbatim into code-analyzer, code-looper, code-workers, code-engineer ──
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

const ERR = (m) => ({ exit: 'error', error: 'code-workers: ' + m, route: 'code-workers', decisions: [], report: '' })
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const ARGS = ['cwd', 'packages', 'paths', 'slice', 'fix', 'minFix', 'focus', 'known', 'fast', 'maxIterations', 'gate', 'polish', 'steps', 'profile', 'provider', 'profiles', 'kind', 'lineBudget']
if (stArgs(a, ARGS)) return ERR(stArgs(a, ARGS))
const CWD = a.cwd || ''
const RAWP = Array.isArray(a.paths) ? a.paths.filter(Boolean).map((p) => String(p).replace(CWD + '/', '').replace(/\/+$/, '')) : []
// A .go path groups under its directory; any other path is taken as a directory.
const dirOf = (p) => (/\.go$/.test(p) ? p.split('/').slice(0, -1).join('/') || '.' : p)
const FILES = {}
for (const p of RAWP) if (/\.go$/.test(p)) (FILES[dirOf(p)] = FILES[dirOf(p)] || []).push(p)
let PKGS = Array.isArray(a.packages) && a.packages.length ? a.packages.filter(Boolean) : [...new Set(RAWP.map(dirOf))]
if (!CWD || !PKGS.length) return ERR('args.cwd and one of args.packages or args.paths are required')
if (a.gate !== undefined && (typeof a.gate !== 'string' || !a.gate.trim())) return ERR('args.gate must be a non-empty command string')
if (a.kind !== undefined && !['feature', 'refactor', 'fix', 'audit', 'consolidate'].includes(a.kind)) return ERR('args.kind must be feature|refactor|fix|audit|consolidate')
if (a.lineBudget !== undefined && !['shrink', 'neutral', 'free'].includes(a.lineBudget)) return ERR('args.lineBudget must be shrink|neutral|free')
if (a.profiles !== undefined && !(Array.isArray(a.profiles) && a.profiles.length && a.profiles.every((x) => /^[A-Za-z0-9._-]+$/.test(String(x))))) return ERR('args.profiles must be a non-empty list of plain names')
const JOB = a.kind || 'fix'
const LINE_BUDGET = a.lineBudget || (JOB === 'refactor' ? 'neutral' : JOB === 'consolidate' ? 'shrink' : 'free')
// ── Steps: who runs each stage; under provider databricks with no args.profile, packages take PROFILES round-robin ──
const STAGES = ['find', 'rederive', 'fix', 'test', 'gate', 'dedupe', 'simplify', 'review']
const PROFILES = a.provider === 'databricks' && !a.profile ? (a.profiles || ['dev', 'qa', 'prod']).map(String) : []
const stagesFor = (profile) => {
  const OPT = { allowed: STAGES, external: true, profile, provider: a.provider }
  return {
    find: stepOf(a.steps, 'find', { model: 'sonnet', effort: 'medium' }, OPT),
    rederive: stepOf(a.steps, 'rederive', { model: 'sonnet', effort: 'medium' }, OPT),
    fix: stepOf(a.steps, 'fix', { model: 'sonnet', effort: 'medium', tools: 'edit' }, OPT),
    test: stepOf(a.steps, 'test', { model: 'sonnet', effort: 'medium', tools: 'edit' }, OPT),
    gate: stepOf(a.steps, 'gate', { model: 'haiku', effort: 'low', tools: 'edit' }, OPT),
    dedupe: stepOf(a.steps, 'dedupe', { model: 'sonnet', effort: 'medium', tools: 'edit' }, OPT),
    simplify: stepOf(a.steps, 'simplify', { model: 'sonnet', effort: 'medium', tools: 'edit' }, OPT),
    review: stepOf(a.steps, 'review', { model: 'sonnet', effort: 'high' }, OPT),
  }
}
let S
let SP = [] // per-profile stage sets for the round-robin; empty = every package runs on S
try {
  S = stagesFor(a.profile)
  SP = PROFILES.map(stagesFor)
} catch (e) { return ERR(e.message) }
for (const k of ['profile', 'provider']) if (a[k] !== undefined && !/^[A-Za-z0-9._-]+$/.test(String(a[k]))) return ERR('args.' + k + ' must be a plain name')
const EXT = [...new Map([S, ...SP].flatMap((x) => Object.values(x)).filter((s) => s.ext).map((s) => [s.agent + '|' + s.profile + '|' + s.provider, s])).values()]
if (EXT.length) {
  const pf = await agent(
    'For each line, run it and report its candidate count and the first candidate as agent/account/model. Edit nothing.\n' +
      EXT.map((s) => `claude-code gateway --agent ${s.agent} --fallback agents${s.profile ? ' --profile ' + s.profile : ''}${s.provider ? ' --provider ' + s.provider : ''} --format json </dev/null`).join('\n'),
    { label: 'coworkers-preflight', phase: 'Preflight', model: 'haiku', effort: 'low', schema: { type: 'object', additionalProperties: false, required: ['routes'], properties: { routes: { type: 'array', items: { type: 'object', additionalProperties: false, required: ['agent', 'candidates', 'first'], properties: { agent: { type: 'string' }, candidates: { type: 'integer' }, first: { type: 'string' }, error: { type: 'string' } } } } } } },
  )
  for (const r of (pf && pf.routes) || []) log(`coworkers preflight ${r.agent}: ${r.candidates} route(s), first ${r.first}${r.error ? ' — ' + r.error : ''}`)
  const routes = (pf && pf.routes) || []
  // fail closed: a missing preflight, or no route row for an external agent, counts as no route
  const dead = [...routes.filter((r) => r.candidates === 0).map((r) => r.agent + (r.error ? ' (' + r.error + ')' : '')),
    ...[...new Set(EXT.map((s) => s.agent))].filter((g) => !routes.some((r) => String(r.agent).includes(g))).map((g) => g + ' (no preflight result)')]
  if (dead.length) return ERR('coworkers: no route for ' + dead.join(', ') + '; BLOCKED before any work')
}
const FIX = a.fix !== false
const POLISH = a.polish !== false
const ORDER = { critical: 0, high: 1, medium: 2, low: 3 }
const MIN_FIX = ORDER[a.minFix] !== undefined ? ORDER[a.minFix] : ORDER.low
const MAX_ITER = Number.isInteger(a.maxIterations) && a.maxIterations >= 0 ? a.maxIterations : ST_GATE_MAX
const MIN_SEV = ORDER[a.minFix] !== undefined ? a.minFix : 'low'
// known: bugs the caller already named; their package skips Find and Rederive (a.fast === false or fix:false disables).
const KNOWN = {}
if (FIX && a.fast !== false && Array.isArray(a.known)) {
  // a known entry is always a file: key it by its directory whatever the language (dirOf treats non-.go paths as dirs)
  for (const k of a.known) if (k && k.file && k.symptom) { const d = stDir(stRel({ cwd: CWD }, k.file)); (KNOWN[d] = KNOWN[d] || []).push(k) }
}
const FOCUS = a.focus ? ' Focus for this group: ' + a.focus + '.' : ''
const RULES =
  'Shared live tree, other packages are being edited in parallel: never git stash|checkout|reset|restore|clean|commit|add, never ' +
  'make build|install|audit, never edit settings.json. cwd: ' + CWD + ' (external agents: ask --workdir ' + CWD + '). Prefix every shell command with `cd ' + CWD + ' &&` (each Bash call starts in the session dir; the copilot shell allows a cd inside its roots).'
// polish:false means a code-workers child of code-engineer: exported-signature and cross-package needs go back as seams
const CTX = { cwd: CWD, cd: '', rules: RULES, S, gateMax: MAX_ITER, gate: a.gate, kind: JOB, lineBudget: LINE_BUDGET, seams: !POLISH }
// sOf/ctxOf: package p's stage set (its round-robin profile, else S); a dir outside PKGS (a re-routed finding) runs on S
const sOf = (p) => (SP.length && PKGS.indexOf(p) >= 0 ? SP[PKGS.indexOf(p) % SP.length] : S)
const ctxOf = (p) => ({ ...CTX, S: sOf(p) })
// ro: test files are skipped unless the focus, lens or finding is about tests.
const ro = (hint) => RULES + ' Read-only: edit nothing, never run go test.' + (/test/i.test((a.focus || '') + ' ' + hint) ? ' *_test.go files are in scope.' : ' Skip *_test.go.')

const SEV = { type: 'string', enum: ['critical', 'high', 'medium', 'low'] }
// kind: a bug has a failing scenario; a cost (stCost) has none, only a real cost and a safe fix.
const KIND = { type: 'string', enum: ['bug', 'perf', 'runtime', 'complexity', 'test-quality', 'duplicate', 'dead', 'legacy', 'stale-test'], default: 'bug' }
const MERGE_KINDS = ['duplicate', 'dead', 'legacy', 'stale-test'] // consolidate findings: no failing scenario, a twin or an empty grep
const FINDING = {
  type: 'object',
  properties: { file: { type: 'string' }, line: { type: 'integer' }, symbol: { type: 'string' }, symptom: { type: 'string' }, scenario: { type: 'string' }, severity: SEV, kind: KIND },
  required: ['file', 'line', 'symbol', 'symptom', 'scenario', 'severity'],
}
const FINDINGS = { type: 'object', properties: { findings: { type: 'array', items: FINDING } }, required: ['findings'] }
const REDERIVE = { type: 'object', properties: { reproduced: { type: 'boolean' }, scenario: { type: 'string' }, severity: SEV }, required: ['reproduced', 'scenario', 'severity'] }
const LENSES = JOB === 'consolidate' ? [
  { key: 'duplicate', ask: 'duplicated helpers or logic (inside the package or against a sibling), legacy paths and shims no caller needs' },
  { key: 'dead', ask: 'dead code with no caller (verify with grep), stale or redundant tests that pin nothing the other tests miss' },
] : [
  { key: 'correctness', ask: 'wrong results, bad parsing, state that disagrees with itself, broken invariants' },
  { key: 'failure', ask: 'swallowed or mishandled errors, partial writes, records or locks left behind, races, leaked secrets' },
]
const norm = (f) => String(f).replace(CWD + '/', '')

function merge(raw) {
  const out = []
  for (const f of raw) {
    const dup = out.find((g) => sameBug(CTX, g, f))
    if (!dup) out.push({ ...f, file: norm(f.file) })
    else if (ORDER[f.severity] < ORDER[dup.severity]) dup.severity = f.severity
  }
  return out
}

// judge: bugs need a derived failing scenario; costs need the cost cited in code and a fix that keeps behavior.
const judge = (f) => MERGE_KINDS.includes(f.kind)
  ? 'It is a consolidation candidate, not a bug. Cite the twin it duplicates or the empty caller grep; check that merging or deleting it keeps behaviour, messages and exit codes identical. ' +
    'reproduced=true only when both hold; put the citation and why the merge is safe in scenario, and rate its severity.'
  : stCost(f)
  ? 'It has no failing scenario by nature. Check the cost is real: cite the spawn, loop, sleep or complexity line (measure it if cheap). ' +
    'Check the fix keeps behavior. reproduced=true only when the cost is real and the change is safe; put the cited line and why the fix is safe in scenario, and rate its severity.'
  : 'Derive your own failing scenario from the code and rate its severity; reproduced=false if you cannot.'

// findAsk: the finder's ask; consolidate looks for merge-and-delete candidates, never bugs.
const findAsk = (l) => JOB === 'consolidate'
  ? ' Find consolidation candidates (not bugs) of this kind: ' + l.ask + '.' + FOCUS + ' Each finding: file, line, symbol, a one-line symptom, kind duplicate|dead|legacy|stale-test, ' +
    'and as its scenario the twin it duplicates or the empty caller grep. Return {findings: []} when clean.'
  : ' Find real bugs of this kind: ' + l.ask + '.' + FOCUS + ' Each finding: file, line, function or symbol, a one-line symptom, a concrete failing scenario, kind bug. A perf, runtime, complexity or ' +
    'test-quality cost with no failing scenario takes that kind and names the cost (spawn, loop, sleep, complexity) as its scenario. Return {findings: []} when clean.'

async function review(p) {
  if (KNOWN[p]) {
    const confirmed = KNOWN[p].map((k) => ({ file: norm(k.file), line: 0, symbol: 'named by the task', symptom: String(k.symptom), scenario: String(k.symptom), severity: ORDER[k.severity] !== undefined ? k.severity : MIN_SEV, rederived: 'named by the task' }))
    log(p + ': fast path, ' + confirmed.length + ' named, find+rederive skipped')
    return { pkg: p, merged: confirmed.length, confirmed, dropped: [], unverified: [], fast: true }
  }
  const St = sOf(p)
  const finds = await parallel(LENSES.map((l) => () =>
    agent(
      St.find.task(ro(l.ask) + ' Package ' + p + ' (its own directory and nested packages).' + (FILES[p] ? ' Focus on these files: ' + FILES[p].join(', ') + '.' : '') + findAsk(l)),
      { phase: 'Find', label: 'find:' + l.key + ':' + p, schema: St.find.schema(FINDINGS), model: St.find.model, effort: St.find.effort, ...St.find.type },
    ).then((r) => St.find.done('find:' + l.key + ':' + p, r))))
  const merged = merge(finds.filter(Boolean).flatMap((r) => r.findings || []))
  const blocked = finds.filter((r) => !r).length // BLOCKED or empty find legs: over half means the package was not really searched
  const judged = await parallel(merged.map((f, i) => () =>
    agent(
      St.rederive.task(ro(f.file + ' ' + (f.kind || '')) + ' A teammate says there is ' + (stCost(f) ? 'a ' + f.kind + ' cost' : 'a bug') + ' at ' + f.file + ':' + f.line + ' (' + f.symbol + ') with this symptom: ' + f.symptom +
        '. You have not seen their reasoning. ' + judge(f)),
      { phase: 'Rederive', label: 'rederive:' + p + ':' + i, schema: St.rederive.schema(REDERIVE), model: St.rederive.model, effort: St.rederive.effort, ...St.rederive.type },
    ).then((r) => ({ f, r: St.rederive.done('rederive:' + p + ':' + i, r) }))))
  // A null rederive (rate or API error) is unverified, never refuted.
  const ok = merged.map((f, i) => judged[i] || { f, r: null })
  const unverified = ok.filter((x) => !x.r).map((x) => ({ file: x.f.file, line: x.f.line, symptom: x.f.symptom }))
  const repro = ok.filter((x) => x.r && x.r.reproduced)
  const confirmed = (await parallel(repro.map((x, i) => () => settleSeverity(p, x, i))))
    .map((c, i) => c || { ...repro[i].f, kind: repro[i].f.kind || 'bug', severity: repro[i].r.severity, rederived: repro[i].r.scenario })
  const dropped = ok.filter((x) => x.r && !x.r.reproduced).map((x) => ({ file: x.f.file, line: x.f.line, symptom: x.f.symptom }))
  log(p + ': merged ' + merged.length + ', confirmed ' + confirmed.length + ', dropped ' + dropped.length + ', unverified ' + unverified.length)
  return { pkg: p, merged: merged.length, confirmed, dropped, unverified, starved: blocked * 2 > finds.length ? blocked + '/' + finds.length : '' }
}

// Severity drift: finder and rederiver split across the fix line → one blind tie-breaker; the median of three wins.
async function settleSeverity(p, x, i) {
  const out = { ...x.f, kind: x.f.kind || 'bug', severity: x.r.severity, rederived: x.r.scenario }
  const fixes = (s) => ORDER[s] <= MIN_FIX
  if (fixes(x.f.severity) === fixes(x.r.severity)) return out
  const St = sOf(p)
  const t = St.rederive.done('tiebreak:' + p + ':' + i, await agent(
    St.rederive.task(ro(x.f.file + ' ' + (x.f.kind || '')) + ' Rate the severity of a possible ' + (stCost(x.f) ? x.f.kind + ' cost' : 'bug') + ' at ' + x.f.file + ':' + x.f.line + ' (' + x.f.symbol + '): ' + x.f.symptom +
      '. ' + judge(x.f)),
    { phase: 'Rederive', label: 'tiebreak:' + p + ':' + i, schema: St.rederive.schema(REDERIVE), model: St.rederive.model, effort: St.rederive.effort, ...St.rederive.type },
  ).catch(() => null))
  const votes = [x.f.severity, x.r.severity, t ? (t.reproduced ? t.severity : 'low') : null].filter(Boolean).sort((a, b) => ORDER[a] - ORDER[b])
  out.severity = votes.length === 3 ? votes[1] : votes[0] // no tie-breaker: the higher rating stands
  out.severityVotes = votes
  log(p + ': severity split ' + x.f.severity + '/' + x.r.severity + ' at ' + x.f.file + ':' + x.f.line + ' → ' + out.severity)
  return out
}

// inPkg: a finder may cite a file outside its package; that finding is fixed by the file's own package, never by p.
// '.' (root files grouped by dirOf) audits the whole tree with its nested dirs, so it owns every relative path.
const inPkg = (file, p) => norm(p) === '.' ? !norm(file).startsWith('/') : norm(file).startsWith(norm(p) + '/')

async function repair(rev) {
  const p = rev.pkg
  const cs = rev.confirmed.filter((c) => ORDER[c.severity] <= MIN_FIX && inPkg(c.file, p))
  if (!FIX) return { ...rev, fixed: 0, decisions: [] }
  if (!cs.length && rev.starved) return { ...rev, fixed: 0, decisions: [p + ': ' + rev.starved + ' find legs BLOCKED — red, not clean'] }
  if (!cs.length) return { ...rev, fixed: 0, decisions: ['no finding at or above minFix in ' + p] }
  log(p + ': ' + cs.length + ' finding(s) at or above minFix, fixing now')
  const fx = await stageRepair(ctxOf(p), p, cs.map((c) => ({ ...c, scenario: c.rederived || c.scenario })), { label: p, gate: 'gate:' + p, scope: norm(p) })
  const { gate, green } = fx
  log(p + ': fixed ' + (fx.done ? cs.length : 0) + '/' + cs.length + ', gate ' + (green ? 'green' : 'red'))
  const decisions = [...(rev.fast ? ['fast path: skipped find+rederive, task names the bug'] : []),
    (fx.done ? 'fixed ' : 'fix NOT done (' + (fx.impl ? fx.impl.status : 'no result') + ') for ') + cs.length + ' finding(s) in ' + p,
    gate ? 'gate ' + gate.exit + (gate.iterations ? ' after ' + gate.iterations + ' fix iteration(s)' : '') + ' in ' + p : 'gate skipped in ' + p + ': fix not done',
    ...(fx.done && !fx.testOk ? ['test stage not done (' + (fx.test ? fx.test.status : 'no result') + ') in ' + p] : [])]
  // fixed counts only a finished fix; attempted keeps a blocked or partial fix red
  return { ...rev, fixed: fx.done ? cs.length : 0, attempted: cs.length, impl: fx.impl, test: fx.test, gate, green, files: fx.files, decisions }
}

// Estimate: 2 finders + ~4 rederivers per package, plus 3 per package that gets fixed.
const SLICE = Number.isInteger(a.slice) && a.slice > 0 ? a.slice : 0
const ALL = PKGS
if (SLICE) PKGS = ALL.slice(0, SLICE)
log('code-workers: ' + PKGS.length + ' package(s), est. ' + (PKGS.length * 6) + ' agents' + (FIX ? ' (+3 per fixed package, up to ' + (PKGS.length * 3) + ', + up to ' + MAX_ITER + ' gate fix iteration(s) each; polish once after the stream: +2-3, +3 per package with review findings)' : '') +
  (SLICE && ALL.length > SLICE ? '; slice=' + SLICE + ' skipped ' + (ALL.length - SLICE) + ': ' + ALL.slice(SLICE).join(', ') : ''))
const results = (await pipeline(PKGS, review, repair)).filter(Boolean)
// Findings cited outside their finder's package: fixed per owning dir after the stream, or under polish:false handed to the parent.
const foreign = {}
for (const r of results) for (const c of r.confirmed) if (!inPkg(c.file, r.pkg) && ORDER[c.severity] <= MIN_FIX) (foreign[dirOf(norm(c.file))] = foreign[dirOf(norm(c.file))] || []).push(c)
const handBack = POLISH ? [] : Object.values(foreign).flat()
for (const dir of POLISH ? Object.keys(foreign) : []) {
  if (!FIX) break
  const fr = await repair({ pkg: dir, merged: 0, confirmed: foreign[dir], dropped: [], unverified: [] })
  fr.confirmed = []
  fr.decisions = ['re-routed ' + foreign[dir].length + ' finding(s) cited outside their finder\'s package to ' + dir, ...(fr.decisions || [])]
  results.push(fr)
}
const confirmed = results.flatMap((r) => r.confirmed).sort((x, y) => ORDER[x.severity] - ORDER[y.severity])
const unverified = results.flatMap((r) => r.unverified || [])
const decisions = results.flatMap((r) => r.decisions || [])
if (handBack.length) decisions.push('returned ' + handBack.length + ' finding(s) cited outside ' + PKGS.join(', ') + ' to the caller (polish:false): ' + Object.keys(foreign).join(', '))
// Polish once over the merged diff of the packages that went green (D3); code-engineer passes polish:false and polishes itself.
const polishFiles = results.filter((r) => r.fixed && r.green).flatMap((r) => r.files || [])
let polish = null
if (FIX && POLISH && polishFiles.length && lowBudget()) decisions.push('polish skipped: budget under 150k tokens remaining')
else if (FIX && POLISH && polishFiles.length) {
  polish = await stagePolish(CTX, { files: polishFiles })
  const fs = polish.review.findings
  const sev = ['critical', 'high', 'medium', 'low'].map((k) => [k, fs.filter((f) => f.severity === k).length]).filter((x) => x[1])
  const tally = 'review: ' + fs.length + ' finding(s) (' + (sev.map((x) => x[1] + ' ' + x[0]).join(', ') || 'none') + ')'
  decisions.push(...polish.decisions.map((l) => (polish.review.ran && /^review: \d/.test(l) ? tally : l)))
} else if (FIX && polishFiles.length) decisions.push('polish skipped: polish=false')
const files = [...new Set([...results.flatMap((r) => r.files || []), ...(polish ? polish.files : [])])].sort() // a partial fix's edits count too
// red reads the last gate per dir: a stage's red stands unless a later gate proved the dir green; an ungated touched dir is red
const selfRed = [...results.filter((r) => (r.attempted && !r.green) || (!r.attempted && r.starved)).map((r) => r.pkg), ...(polish ? polish.red : [])]
const touched = files.filter((f) => a.gate || /\.go$/.test(f)).map((f) => stDir(norm(f)))
const red = [...new Set([...stGateRed([...ST_GATES.map((g) => g.dir), ...touched, ...selfRed]), ...(polish ? polish.hold : [])])].sort()
if (ST_OVER.length) decisions.push('over line budget ' + LINE_BUDGET + ': ' + ST_OVER.map((o) => o.pkg + ' net ' + o.net).join(', ') + (red.length ? '' : ' — exit review'))
const lines = stLines(ST_LINES)
const fixed = results.filter((r) => r.fixed).map((r) => ({ pkg: r.pkg, findings: r.fixed, impl: r.impl, test: r.test, gate: r.gate, files: r.files || [] }))
if (!FIX) decisions.push('review only: fix=false')
if (unverified.length) decisions.push(unverified.length + ' finding(s) unverified: rederive agent returned nothing')
if (SLICE && ALL.length > SLICE) decisions.push('slice=' + SLICE + ': skipped ' + (ALL.length - SLICE) + ' package(s)')
const bullets = confirmed.map((c) => '- **' + c.severity + '** `' + c.file + ':' + c.line + '` (' + c.symbol + ') — ' + c.symptom)
const report = ['# code-workers: ' + confirmed.length + ' confirmed finding(s) in ' + PKGS.length + ' package(s)', ...bullets,
  '', 'Fixed: ' + fixed.map((f) => f.pkg + ' (' + f.findings + ')').join(', ') + (fixed.length ? '' : 'none') + '. Red: ' + (red.join(', ') || 'none') + '.',
  ...(unverified.length ? ['Unverified (rederive failed): ' + unverified.map((u) => u.file + ':' + u.line).join(', ') + '.'] : [])].join('\n') + (polish ? polish.report : '') +
  (ST_LINES.length || ST_PROPOSED.length ? '\n' + stTail(lines, ST_OVER, ST_PROPOSED) : '')
return {
  exit: red.length ? 'red' : !FIX || ST_OVER.length ? 'review' : 'green',
  route: 'code-workers',
  packages: PKGS.length,
  merged: results.reduce((n, r) => n + r.merged, 0),
  confirmed,
  dropped: results.flatMap((r) => r.dropped),
  unverified,
  fixed,
  red,
  polish,
  files,
  foreign: handBack,
  seams: ST_SEAMS,
  lines,
  proposedTests: ST_PROPOSED,
  over: ST_OVER,
  gates: ST_GATES,
  decisions,
  report,
  coworkers: COWORKERS,
}
