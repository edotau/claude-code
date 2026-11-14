export const meta = {
  name: 'code-engineer',
  description: 'One size-routed entry point: analytical first when the task needs it (code-analyzer scans an audit or range, code-evaluator scores a skill), then one code-workers child per top-level dir fixes (waves of cap), code-looper loops on a red gate or a multi-child run; then one polish (an epic drafts a plan)',
  whenToUse:
    'USE: any code job when you do not want to pick a workflow; sizes it (TRIVIAL/LOW/MEDIUM/HIGH/EPIC) and routes. CALL: Workflow({scriptPath: "~/.claude/workflows/code-engineer.js", args: {cwd, task?, packages?, paths?, range?, skill?, plan?}}) with at least one of task/packages/paths/range/skill/plan (range = a git range like main..HEAD; a bare range is scanned by code-analyzer and its findings fixed by code-workers; with a task, paths or packages its changed Go files become paths). OPTIONAL: kind (feature|refactor|fix|audit|consolidate; wins over the size agent and the task-verb guess; feature and refactor get an implementer, which exits incomplete when it changes no file and review when it reports no live run; feature also gets ONE milliseconds-fast test; consolidate finds duplicates, dead code, legacy and stale or redundant tests, not bugs, and merges and deletes them with behaviour, messages and exit codes unchanged), lineBudget (shrink: production net ≤ 0 | neutral: ≤ +20 per package | free; default by kind: refactor neutral, consolidate shrink, else free; numstat-measured, an over-budget fix gets one shrink-or-justify pass, still over exits review; Dedupe and Simplify are always held to net ≤ 0 and report partial otherwise), tier (skips sizing), fix (default true), minFix (default low: every finding is fixed, decision #54), focus, dimensions (code-analyzer lenses on a scan route), label (report file name, default code-engineer), record (default true; false skips the Record step), steps {size, intel, impl, plan, runner, dedupe, simplify, review, record, child} = {agent?, model?, effort?, profile?, provider?} per stage (agent claude|codex|copilot|gemini|opencode; external stages run on their pins, or args/steps profile|provider, and fall back on 429/auth; each reports ran on or BLOCKED). OPTIONAL: profile, provider (run-wide override for external stages; profile reaches every child), profiles (default [dev, qa, prod]: under provider databricks with no profile the children take them round-robin), cap (children per wave, default 16, max 24). An unknown arg or steps key is refused at launch. child = the routed child\'s own steps (simplify/review/dedupe are rejected there: code-engineer polishes once); its fix, test and gate also drive the polish Fix-all. Fan-out: one code-workers child per top-level dir (polish:false), launched in waves of cap; a child never changes an exported signature or edits outside its dir: it reports seams, and one serial Seam step applies them (with every caller) after each wave. gate (a shared gate command) with gateScope (dirs its fixer may edit, disjoint from the groups): one code-looper child runs beside them (none under fix:false); the shared gate is re-checked once, check-only, after every child and the polish (a clean scan included). gate without gateScope: every package\'s gate instead (passed to the code-workers children, the loop and code-engineer\'s own repairs; e.g. npm test for JS/TS). Findings a child cites outside its group come back to code-engineer, which repairs them once after the children. skill (+ base, scenarios, seeds, conditions): a skill/route comparison, routed to code-evaluator. plan (a plan file, exclusive with task/packages/paths/range/skill, fix:false and gateScope): route plan hands it to `claude-code agents dispatch --waves` through one background runner leaf (steps.runner) awaited in 10-minute waits, waitRounds times (default 6) then exit incomplete with the runner id; dryPlan lists the packages and launches nothing; profile, provider, overlay and gate reach the runner; exit green|red|error|review|incomplete from the runner\'s 0|1|2|3, the dirs of a wave whose gate failed go to code-looper, polish runs after a green runner only with polish:true. polish (default true; false skips it) runs once after the children over the merged diff: Dedupe (2+ packages), Simplify, Review; each package with review findings gets one Fix-all, test repair and gate (no second child run); a polished package no Fix-all re-gated gets one gate. Test stages repair what the edits broke and write nothing new (kind feature: one test); every other test comes back in proposedTests with its case, reason and runtime. fast (default true; false disables): a LOW/MEDIUM fix task passes its files to code-workers as known, skipping Find and Rederive. A low token budget (under 150k left) skips the polish. RETURNS {exit (green|red|review|plan|error, or a child\'s blocked|stalled|incomplete|capped; green only when every touched dir\'s last gate is green), tier, route, packages, merged, confirmed, dropped, unverified, fixed, red, files (sorted repo-relative files the implementer, fixers, gate fixers and polish changed), polish, decisions, verdict (at most 8 lines), reportPath} (runs, cells on the code-evaluator route; with record:false the full report, lines (git diff --numstat per stage, per package and total, production vs test), proposedTests, over and gates instead; with a recorded run they are only in the saved report). Details: saved-workflows.md.',
  phases: [
    { title: 'Size', detail: 'one cheap agent reads the task, git status/diff and go list and returns the tier, packages and paths' },
    { title: 'Route', detail: 'MEDIUM adds an intel agent, a feature task adds an implementer, EPIC drafts a plan and stops' },
    { title: 'Run', detail: 'code-evaluator (skill) and code-analyzer scan (audit, package list, bare range; scan:false skips) first; then one code-workers child per top-level dir in waves of cap (known = the scan findings); code-looper over red, ungated or shared dirs (loop:false skips)' },
    { title: 'Seam', detail: 'after each wave, one serial step applies the exported-signature and cross-package changes the children reported' },
    { title: 'Dedupe', detail: 'once over the merged diff, when 2+ packages changed (polish:false skips)' },
    { title: 'Simplify', detail: 'once over the merged diff; review simplify plus the simplify gate' },
    { title: 'Review', detail: 'once over the merged diff; review findings get one Fix-all, test repair and gate per package (no second child run)' },
    { title: 'Fix', detail: 'Fix-all: one implementer per package with review findings, numstat-measured against lineBudget' },
    { title: 'Test', detail: 'repair tests the edits broke; kind feature adds ONE test; every other test is proposed (proposedTests)' },
    { title: 'Gate', detail: 'package-scoped re-gate after Fix-all, up to 2 fixer iterations on red' },
    { title: 'Record', detail: 'one cheap agent appends the report and decisions to the memory store (record:false skips)' },
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

const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const CWD = a.cwd || ''
const TASK = a.task ? String(a.task) : ''
const ERR = (m, tier, route, decisions) => ({ exit: 'error', error: 'code-engineer: ' + m, tier: tier || '', route: route || '', decisions: decisions || [], report: '' })
const ARGS = ['cwd', 'task', 'packages', 'paths', 'range', 'skill', 'kind', 'tier', 'fix', 'minFix', 'focus', 'dimensions', 'label', 'record', 'steps', 'profile', 'provider', 'profiles',
  'gate', 'gateScope', 'polish', 'fast', 'scan', 'loop', 'scripts', 'base', 'scenarios', 'seeds', 'conditions', 'lineBudget', 'cap', 'plan', 'dryPlan', 'waitRounds', 'overlay']
if (stArgs(a, ARGS)) return ERR(stArgs(a, ARGS))
const strs = (v) => (Array.isArray(v) ? v.filter(Boolean).map(String) : [])
const lst = (v) => v || []
const errOf = (r, dflt) => (r && r.error) || dflt
// texts: the first n looper residual rows ({file, line, text}) as their text
const texts = (rs, n) => strs((Array.isArray(rs) ? rs : []).slice(0, n).map((x) => (x && x.text) || ''))
// rels makes every path repo-relative: an absolute and a relative spelling of one dir would become two overlapping children
const rels = (v) => [...new Set(strs(v).map((x) => x.replace(CWD ? CWD + '/' : /^$/, '').replace(/^\.\//, '').replace(/\/+$/, '')).filter(Boolean))]
let PATHS = rels(a.paths)
let PKGS = rels(a.packages)
const TIERS = ['TRIVIAL', 'LOW', 'MEDIUM', 'HIGH', 'EPIC']
const RANGE = a.range ? String(a.range) : ''
const EVAL = !!a.skill
const PLAN_FILE = a.plan ? String(a.plan) : ''
// skill alone is a pure comparison; with code targets, code-evaluator runs first, before any code changes
const EVAL_ONLY = EVAL && !TASK && !PATHS.length && !PKGS.length && !RANGE
if (RANGE && !/^[A-Za-z0-9._\/~^@{}-]+(\.\.\.?[A-Za-z0-9._\/~^@{}-]+)?$/.test(RANGE)) return ERR('args.range must be a git range like main..HEAD')
if (!CWD || !(TASK || PATHS.length || PKGS.length || RANGE || EVAL || PLAN_FILE)) return ERR('args.cwd and one of args.task, args.packages, args.paths, args.range, args.skill or args.plan are required')
if (a.plan !== undefined && !/^[A-Za-z0-9._\/@+=-]+\.(md|jsonl)$/.test(PLAN_FILE)) return ERR('args.plan must be a plan file path ending .md or .jsonl, with no spaces or shell characters')
if (a.dryPlan !== undefined && typeof a.dryPlan !== 'boolean') return ERR('args.dryPlan must be a boolean')
if (a.waitRounds !== undefined && !(Number.isInteger(a.waitRounds) && a.waitRounds > 0 && a.waitRounds <= 24)) return ERR('args.waitRounds must be an integer 1-24 (10-minute waits)')
if (a.overlay !== undefined && !(typeof a.overlay === 'string' && a.overlay && !/[\r\n]/.test(a.overlay))) return ERR('args.overlay must be a one-line string (file or JSON)')
if (a.tier !== undefined && !TIERS.includes(a.tier)) return ERR('args.tier must be ' + TIERS.join('|'))
if (a.kind !== undefined && !['feature', 'refactor', 'fix', 'audit', 'consolidate'].includes(a.kind)) return ERR('args.kind must be feature|refactor|fix|audit|consolidate')
if (a.lineBudget !== undefined && !['shrink', 'neutral', 'free'].includes(a.lineBudget)) return ERR('args.lineBudget must be shrink|neutral|free')
if (a.cap !== undefined && !(Number.isInteger(a.cap) && a.cap > 0 && a.cap <= 24)) return ERR('args.cap must be an integer 1-24 (children per wave)')
if (a.profiles !== undefined && !(Array.isArray(a.profiles) && a.profiles.length && a.profiles.every((x) => /^[A-Za-z0-9._-]+$/.test(String(x))))) return ERR('args.profiles must be a non-empty list of plain names')
// waves of CAP children; under provider databricks with no args.profile each child takes the next of PROFILES (round-robin)
const CAP = a.cap || 16
const MIN_FIX = a.minFix || 'low' // the scan's severity floor
const PROFILES = a.provider === 'databricks' && !a.profile ? (a.profiles || ['dev', 'qa', 'prod']).map(String) : []
const profileOf = (i) => (PROFILES.length ? PROFILES[i % PROFILES.length] : a.profile)
// A bare range (no task, paths or packages) is a diff audit, so code-analyzer owns it (D2).
const ANALYZE = !!RANGE && !TASK && !PATHS.length && !PKGS.length
// args.gate + gateScope: a shared gate a code-looper child keeps green beside the children, editing gateScope.
// args.gate alone: every package's gate, passed to the code-workers children and used by code-engineer's own repairs.
const GATE_SCOPE = strs(a.gateScope).map((d) => d.replace(/\/+$/, ''))
const GATE = a.gate && GATE_SCOPE.length ? String(a.gate) : ''
const PKG_GATE = a.gate && !GATE_SCOPE.length ? String(a.gate) : undefined
if (PLAN_FILE && (TASK || PATHS.length || PKGS.length || RANGE || EVAL || GATE_SCOPE.length || a.fix === false)) return ERR('args.plan runs a plan only: not with task, paths, packages, range, skill, gateScope or fix:false')
// steps.child is the routed child's own steps; the rest are ours.
const { child: CHILD_RAW, ...OWN } = a.steps && typeof a.steps === 'object' && !Array.isArray(a.steps) ? a.steps : {}
// a fix task never runs impl (code-workers' fix stage does the edit), so steps.impl drives that fix stage unless child.fix is set
const CHILD_STEPS = OWN.impl && !(CHILD_RAW && CHILD_RAW.fix) ? { ...(CHILD_RAW || {}), fix: OWN.impl } : CHILD_RAW
for (const k of ['simplify', 'review', 'dedupe']) if (CHILD_STEPS && CHILD_STEPS[k]) return ERR('steps.child.' + k + ': code-engineer runs polish once; set steps.' + k)
// the child's stages are code-workers' (the dispatch hook checks the same list): an unknown key is refused, never dropped
const CHILD_STAGES = ['find', 'rederive', 'fix', 'test', 'gate']
for (const k of Object.keys(CHILD_STEPS || {})) if (!CHILD_STAGES.includes(k)) return ERR('steps.child.' + k + ': unknown stage (allowed: ' + CHILD_STAGES.join('|') + ')')
const OPT = { external: true, profile: a.profile, provider: a.provider, allowed: ['size', 'intel', 'impl', 'plan', 'runner', 'dedupe', 'simplify', 'review', 'record'] }
let S
let CS // the child's fix, test and gate stages, run in-script by the polish Fix-all
try {
  S = {
    size: stepOf(OWN, 'size', { model: 'haiku', effort: 'low' }, OPT),
    intel: stepOf(OWN, 'intel', { model: 'sonnet', effort: 'medium' }, OPT),
    impl: stepOf(OWN, 'impl', { tools: 'edit', model: 'sonnet', effort: 'medium' }, OPT),
    plan: stepOf(OWN, 'plan', { model: 'sonnet', effort: 'medium' }, OPT),
    runner: stepOf(OWN, 'runner', { tools: 'run', model: 'haiku', effort: 'low' }, OPT),
    dedupe: stepOf(OWN, 'dedupe', { tools: 'edit', model: 'sonnet', effort: 'medium' }, OPT),
    simplify: stepOf(OWN, 'simplify', { tools: 'edit', model: 'sonnet', effort: 'medium' }, OPT),
    review: stepOf(OWN, 'review', { model: 'sonnet', effort: 'high' }, OPT),
    record: stepOf(OWN, 'record', { tools: 'edit', model: 'haiku', effort: 'low' }, OPT),
  }
  const CO = { ...OPT, allowed: CHILD_STAGES }
  CS = {
    fix: stepOf(CHILD_STEPS, 'fix', { tools: 'edit', model: 'sonnet', effort: 'medium' }, CO),
    test: stepOf(CHILD_STEPS, 'test', { tools: 'edit', model: 'sonnet', effort: 'medium' }, CO),
    gate: stepOf(CHILD_STEPS, 'gate', { tools: 'edit', model: 'haiku', effort: 'low' }, CO),
  }
} catch (e) { return ERR(e.message) }
for (const k of ['profile', 'provider']) if (a[k] !== undefined && !/^[A-Za-z0-9._-]+$/.test(String(a[k]))) return ERR('args.' + k + ' must be a plain name')
const EXT = [...new Map(Object.values(S).filter((s) => s.ext).map((s) => [s.agent + '|' + s.profile + '|' + s.provider, s])).values()]
let HOME = '' // from the preflight, else read once by runChild
if (EXT.length) {
  const pf = await agent(
    'For each line, run it and report its candidate count and the first candidate as agent/account/model. Edit nothing. First run `echo $HOME` and report it as home.\n' +
      EXT.map((s) => `claude-code gateway --agent ${s.agent} --fallback agents${s.profile ? ' --profile ' + s.profile : ''}${s.provider ? ' --provider ' + s.provider : ''} --format json </dev/null`).join('\n'),
    { label: 'coworkers-preflight', phase: 'Preflight', model: 'haiku', effort: 'low', schema: { type: 'object', additionalProperties: false, required: ['home', 'routes'], properties: { home: { type: 'string' }, routes: { type: 'array', items: { type: 'object', additionalProperties: false, required: ['agent', 'candidates', 'first'], properties: { agent: { type: 'string' }, candidates: { type: 'integer' }, first: { type: 'string' }, error: { type: 'string' } } } } } } },
  )
  HOME = (pf && pf.home) || ''
  for (const r of (pf && pf.routes) || []) log(`coworkers preflight ${r.agent}: ${r.candidates} route(s), first ${r.first}${r.error ? ' — ' + r.error : ''}`)
  const routes = (pf && pf.routes) || []
  // fail closed: a missing preflight, or no route row for an external agent, counts as no route
  const dead = [...routes.filter((r) => r.candidates === 0).map((r) => r.agent + (r.error ? ' (' + r.error + ')' : '')),
    ...[...new Set(EXT.map((s) => s.agent))].filter((g) => !routes.some((r) => String(r.agent).includes(g))).map((g) => g + ' (no preflight result)')]
  if (dead.length) return ERR('coworkers: no route for ' + dead.join(', ') + '; BLOCKED before any work')
}

const RULES =
  'Shared live tree: never git stash|checkout|reset|restore|clean|commit|add, never make build|install|audit, never edit settings.json. ' +
  'cwd: ' + CWD + ' (external agents: ask --workdir ' + CWD + '). Prefix every shell command with `cd ' + CWD + ' &&` (each Bash call starts in the session dir; the copilot shell allows a cd inside its roots).'
let CTX = { cwd: CWD, cd: '', rules: RULES, S: { ...CS, dedupe: S.dedupe, simplify: S.simplify, review: S.review }, gateMax: ST_GATE_MAX, gate: PKG_GATE } // kind, lineBudget set after Size
const SIZED = {
  type: 'object',
  properties: {
    kind: { type: 'string', enum: ['fix', 'feature', 'refactor', 'audit', 'consolidate', 'architecture'] },
    packages: { type: 'array', items: { type: 'string' } }, paths: { type: 'array', items: { type: 'string' } },
    focus: { type: 'string' }, reason: { type: 'string' },
  },
  required: ['kind', 'packages', 'paths', 'focus', 'reason'],
}
// tierOf is the deterministic size rule: the agent only finds files and the job kind, the script picks the tier.
const tierOf = (kind, paths, pkgs) => {
  if (kind === 'architecture') return 'EPIC'
  const files = paths.filter((p) => /\.[A-Za-z0-9]+$/.test(p))
  if (kind === 'audit' || (kind === 'consolidate' && !files.length)) return 'HIGH' // a package-level sweep, no named files
  const dirs = new Set([...pkgs, ...files.map((p) => p.split('/').slice(0, -1).join('/'))])
  return tierBy(dirs.size, files.length)
}
// tierBy: HIGH past 3 dirs or 8 files, MEDIUM at 2+ of either, else LOW.
const tierBy = (nDirs, nFiles) => (nDirs > 3 || nFiles > 8 ? 'HIGH' : nDirs >= 2 || nFiles >= 2 ? 'MEDIUM' : 'LOW')
// IMPL is ST_WORK plus `live`: the changed surface run on real input before tests, or why there is none.
const IMPL = { ...ST_WORK, properties: { ...ST_WORK.properties, live: { type: 'object', properties: { command: { type: 'string' }, exit: { type: 'integer' }, output: { type: 'string' }, reason: { type: 'string' } }, required: ['command'] } }, required: [...ST_WORK.required, 'live'] }
const INTEL = { type: 'object', properties: { paths: { type: 'array', items: { type: 'string' } }, focus: { type: 'string' } }, required: ['paths', 'focus'] }
const PLAN = { type: 'object', properties: { plan: { type: 'string' } }, required: ['plan'] }
const PLAN_OUT = { type: 'object', properties: { exit: { type: ['integer', 'null'] }, out: { type: 'string' }, runner: { type: ['integer', 'null'] } }, required: ['exit', 'out'] }
const PLAN_EXIT = { 0: 'green', 1: 'red', 2: 'error', 3: 'review' } // the plan runner's exit codes
const RECORDED = { type: 'object', properties: { paths: { type: 'array', items: { type: 'string' } } }, required: ['paths'] }
const ORDER = { critical: 0, high: 1, medium: 2, low: 3 }
const KEEP = ['blocked', 'stalled', 'incomplete', 'capped'] // child exits merge keeps, worst first

// ── Size ──
phase('Size')
const decisions = []
let IMPL_FILES = [] // declared before any record() call: touchedFiles reads it on the early returns too
let IMPL_EMPTY = false // an implementer ran for a build task but changed no file
let IMPL_NO_LIVE = false // the implementer reported neither a live run nor why it had none
let tier = a.tier
let focus = a.focus || TASK
let KIND = a.kind || '' // args.kind wins over the size agent and the task-verb regex
// A git range with a task, paths or packages becomes the changed Go files (non-test); a bare one goes to code-analyzer.
if (RANGE && !ANALYZE) {
  const ch = S.size.done('range', await agent(
    S.size.task(RULES + ' Read-only. Run `git diff --name-only ' + RANGE + " -- '*.go'` and return the files that still exist and are not *_test.go, " +
      'repo-relative. Return {paths: []} when there are none.'),
    { phase: 'Size', label: 'range', schema: S.size.schema(RECORDED), model: S.size.model, effort: S.size.effort, ...S.size.type },
  ).catch(() => null))
  const got = ch ? strs(ch.paths) : []
  PATHS = [...new Set([...PATHS, ...got])]
  decisions.push('range ' + RANGE + ': ' + got.length + ' changed Go file(s)')
  if (!PATHS.length && !PKGS.length && !TASK) return await record({ exit: 'green', tier: 'TRIVIAL', route: 'none', packages: 0, merged: 0, confirmed: [], dropped: [], unverified: [], fixed: [], red: [], decisions, report: '# code-engineer: range ' + RANGE + ' has no Go changes' })
  if (!tier && !TASK) {
    const dirs = new Set(PATHS.map((p) => p.split('/').slice(0, -1).join('/')))
    tier = tierBy(dirs.size, PATHS.length)
    if (tier === 'HIGH' && !PKGS.length) PKGS = [...dirs]
    decisions.push('sized as ' + tier + ': range spans ' + dirs.size + ' package(s), ' + PATHS.length + ' file(s)')
  }
}
if (PLAN_FILE) {
  tier = 'PLAN'
  decisions.push('plan route: ' + PLAN_FILE + ' (the wave runner owns launch, owned-file check, gates and verdict)')
} else if (ANALYZE || EVAL_ONLY) {
  tier = a.tier || (ANALYZE ? 'HIGH' : 'MEDIUM')
  decisions.push('sized as ' + tier + ': ' + (ANALYZE ? 'a bare range is a diff audit (code-analyzer)' : 'a skill/route comparison (code-evaluator)'))
} else if (!tier && !TASK) {
  tier = PKGS.length ? 'HIGH' : 'LOW'
  decisions.push('sized as ' + tier + ': no task, ' + (PKGS.length ? 'a package list is an audit' : 'only named paths'))
} else if (!tier) {
  const sz = S.size.done('size', await agent(
    S.size.task(RULES + ' Read-only: edit nothing. Do NOT pick a size; find facts. Read the code the task points at and return: kind (fix = repair ' +
      'existing behavior, feature = add behavior, refactor = change structure keeping existing behavior, audit = review packages for bugs, consolidate = merge duplicates and delete dead code, legacy paths or stale tests keeping behavior, architecture = a multi-session design change); the ' +
      'non-test source FILES that must change (repo-relative, one entry per file, never a directory, tests excluded); the Go package dirs ' +
      'only for an audit or consolidate; a one-line focus; and a one-line reason. Use `git status --short` and `go list ./...` only as needed.\nTask: ' + TASK +
      (PKGS.length ? '\nGiven packages: ' + PKGS.join(', ') : '') + (PATHS.length ? '\nGiven paths: ' + PATHS.join(', ') : '')),
    { phase: 'Size', label: 'size', schema: S.size.schema(SIZED), model: S.size.model, effort: S.size.effort, ...S.size.type },
  ).catch(() => null))
  if (!sz) return ERR('sizing agent returned nothing; pass args.tier to skip it')
  KIND = a.kind || sz.kind
  if (!PKGS.length) PKGS = rels(sz.packages)
  if (!PATHS.length) PATHS = rels(sz.paths).filter((p) => !/_test\.go$/.test(p))
  tier = tierOf(sz.kind, PATHS, sz.kind === 'audit' || sz.kind === 'consolidate' ? PKGS : [])
  log('size: kind ' + sz.kind + ', ' + PATHS.length + ' file(s), ' + PKGS.length + ' package(s) -> ' + tier)
  decisions.push('sized as ' + tier + ' (rule: ' + sz.kind + ', ' + PATHS.length + ' file(s)): ' + sz.reason)
  if (!a.focus && sz.focus) focus = sz.focus
} else if (a.tier) decisions.push('sized as ' + tier + ': args.tier given')
// lineBudget: args win; else refactor holds each package to +20 production lines, consolidate to net ≤ 0, the rest are free
const LINE_BUDGET = a.lineBudget || (KIND === 'refactor' ? 'neutral' : KIND === 'consolidate' ? 'shrink' : 'free')
CTX = { ...CTX, kind: KIND, lineBudget: LINE_BUDGET }
if (LINE_BUDGET !== 'free') decisions.push('line budget: ' + LINE_BUDGET + (a.lineBudget ? ' (args)' : ' (kind ' + KIND + ')'))

// ── Route ──
// analytical first: an audit (bare range, package list with no task, or kind audit) scans with code-analyzer, then code-workers fix what it confirmed
const SCAN = a.scan !== undefined ? !!a.scan : ANALYZE || KIND === 'audit' || (!TASK && PKGS.length > 0)
const route = PLAN_FILE ? 'plan' : EVAL_ONLY ? 'code-evaluator' : tier === 'EPIC' ? 'plan' : (EVAL ? 'code-evaluator→' : '') + (SCAN ? 'code-analyzer→' : '') + (tier === 'HIGH' || ANALYZE ? 'code-workers:packages' : 'code-workers:paths')
decisions.push('route: ' + route)
log('route: ' + route + ' (tier ' + tier + ', kind ' + (KIND || 'n/a') + ')')
const head = '# code-engineer: ' + tier + ' → ' + route
phase('Route')
// runChild runs one child workflow one nesting level down; a throw becomes an error result.
// children load from the harness scripts (args.scripts for a worktree copy); a target's own workflows/ dir is unreadable outside the session roots
// workflow() does not expand ~, so the default needs the absolute home
const scriptsDir = async () => {
  if (a.scripts) return String(a.scripts).replace(/\/$/, '')
  if (!HOME) HOME = ((await agent('Run `echo $HOME` and return it as home. Edit nothing.', { label: 'home', phase: 'Route', model: 'haiku', effort: 'low', schema: { type: 'object', required: ['home'], properties: { home: { type: 'string' } } } }).catch(() => null)) || {}).home || ''
  return HOME + '/.claude/workflows'
}
const runChild = async (child, cargs) => workflow({ scriptPath: (await scriptsDir()) + '/' + child + '.js' }, cargs).catch((e) => ({ exit: 'error', error: child + ' threw: ' + e.message }))
if (PLAN_FILE) return await planRoute()
if (EVAL_ONLY) {
  const ev = await runChild('code-evaluator', { base: a.base, skill: a.skill, scenarios: a.scenarios, seeds: a.seeds, conditions: a.conditions, steps: CHILD_STEPS, profile: a.profile, provider: a.provider })
  if (!ev || ev.exit === 'error') return ERR((ev && ev.error) || 'code-evaluator returned nothing', tier, route, decisions)
  const cells = ev.cells || []
  return await record({ exit: 'review', tier, route, packages: 0, merged: 0, confirmed: [], dropped: [], unverified: [], fixed: [], red: [], runs: ev.runs, cells, decisions: [...decisions, 'eval: code-evaluator scored ' + cells.length + ' cell(s); no code changed'], report: head + '\n' + cells.map((c) => '- ' + JSON.stringify(c)).join('\n') })
}
if (tier === 'EPIC') {
  const p = S.plan.done('plan', await agent(
    S.plan.task(RULES + ' Read-only: edit nothing. Draft an implementation plan for this task in the shape of ' + CWD + '/docs/templates/plan.md (read it first), with the ' +
      'real files and phases found in the repo. Return the markdown in `plan`.\nTask: ' + TASK),
    { phase: 'Route', label: 'plan', schema: S.plan.schema(PLAN), model: S.plan.model, effort: S.plan.effort, ...S.plan.type },
  ).catch(() => null))
  if (!p) return ERR('plan agent returned nothing', tier, route, decisions)
  decisions.push('epic: no code changed, plan drafted for the owner')
  return await record({ exit: 'plan', tier, route, packages: 0, merged: 0, confirmed: [], dropped: [], unverified: [], fixed: [], red: [], decisions, report: head + '\n' + p.plan })
}
// HIGH with only files: code-workers packages are their directories, never the file paths (finders roam otherwise).
const dirsOf = (ps) => [...new Set(ps.map((p) => /\.[A-Za-z0-9]+$/.test(p) ? p.split('/').slice(0, -1).join('/') : p))]
// code-workers audits a package with its nested ones, so a child of another listed dir would be audited twice.
const topDirs = (ds) => ds.filter((d) => !ds.some((o) => o !== d && d.startsWith(o + '/')))
const target = tier === 'HIGH' ? { packages: topDirs(PKGS.length ? PKGS : dirsOf(PATHS)) } : { paths: PATHS }
const sized = [...(target.paths || [])] // the files the task names; intel may add context, never new packages to a fix
if (tier === 'MEDIUM' && TASK) {
  const intel = S.intel.done('intel', await agent(
    S.intel.task(RULES + ' Read-only: edit nothing. Codebase intel for this task: structure, conventions, and the exact files to change with their callers. ' +
      'Return `paths` (repo-relative files to change or audit) and `focus` (the task plus the conventions and callers that matter).\nTask: ' + TASK +
      (PATHS.length ? '\nStart from: ' + PATHS.join(', ') : '')),
    { phase: 'Route', label: 'intel', schema: S.intel.schema(INTEL), model: S.intel.model, effort: S.intel.effort, ...S.intel.type },
  ).catch(() => null))
  if (intel && rels(intel.paths).length) {
    const own = new Set(dirsOf(sized))
    const got = KIND === 'fix' && sized.length ? rels(intel.paths).filter((p) => own.has(dirsOf([p])[0])) : rels(intel.paths)
    const dropped = rels(intel.paths).length - got.length
    if (dropped) decisions.push('intel: kept ' + got.length + ' path(s) in the named package(s), dropped ' + dropped + ' outside them (context only)')
    target.paths = [...new Set([...sized, ...got])]
    focus = TASK + '. Intel: ' + intel.focus
  } else decisions.push('intel returned nothing; kept the sized paths')
}
// code-workers only fixes bugs: a feature or refactor task gets an implementer first, then code-workers audits and hardens the new code.
const builds = KIND ? KIND === 'feature' || KIND === 'refactor' : /^\W*(build|add|implement|create|introduce|support|write|finish|refactor|move|migrate|replace|rewire|fold|extract|retire|remove|delete|rename|phase)\b/i.test(TASK) // leading verb only: constraint text mentioning review must not veto
const implTargets = target.paths || (PATHS.length ? PATHS : target.packages) // HIGH builds in the named files, else its package dirs
if (TASK && implTargets.length && builds && a.fix === false) decisions.push('feature task: implementer skipped under fix=false (review only)')
else if (TASK && implTargets.length && builds) {
  const implDirs = dirsOf(implTargets)
  const pre = await stSnap(CTX, 'pre:impl', implDirs)
  const impl = S.impl.done('impl', await agent(
    S.impl.task(RULES + ' Make this change in these files (new files in the same package ok) and nothing else; smallest correct change, functions < 50 lines, 1-line comments. ' +
      'Run gofmt and go vet on the package. Do not write tests; ' + (KIND === 'feature' ? 'one Test stage follows for the feature, then ' : '') + 'code-workers follows with find, fix and gate. Return `files`: every repo-relative file you changed. ' +
      'Then run the changed surface live (the CLI verb, hook, endpoint or make target, e.g. `go run ./cmd/claude-code <verb> ...`) on real input and return `live`: {command, exit, output (trimmed tail)}; no runnable surface: {command: "", reason: "<why>"}.\nFiles: ' + implTargets.join(', ') + '\nTask: ' + TASK + '\nContext: ' + focus),
    { phase: 'Route', label: 'impl', schema: S.impl.schema(IMPL), model: S.impl.model, effort: S.impl.effort, ...S.impl.type },
  ).catch(() => null))
  if (impl) IMPL_FILES = strs(impl.files).map((f) => stRel(CTX, f))
  IMPL_EMPTY = !IMPL_FILES.length
  decisions.push('implementer ran: ' + (impl ? impl.status + ' — ' + impl.summary : 'returned nothing'))
  const live = (impl && impl.live) || {}
  IMPL_NO_LIVE = !live.command && !live.reason
  decisions.push(live.command ? 'implementer live run: `' + live.command + '` exit ' + live.exit : live.reason ? 'implementer live run: none, ' + live.reason : 'implementer reported no live run')
  decisions.push('feature task: code-workers below is an audit-and-harden pass over the new code, not a feature builder')
  await implMeasured(pre, implDirs, impl)
}

// ── Run ──
phase('Run')
// Fast path: a LOW/MEDIUM fix task names the bug, so code-workers skips Find and Rederive for those files (audits and features never).
const fastFiles = (tier === 'LOW' || tier === 'MEDIUM') && KIND === 'fix' && TASK && a.fix !== false && a.fast !== false ? (sized.length ? sized : target.paths || []).filter((p) => /\.go$/.test(p) && !/_test\.go$/.test(p)) : []
const known = fastFiles.map((file) => ({ file, symptom: TASK }))
if (known.length) { decisions.push('fast path: ' + known.length + ' file(s) passed to code-workers as known'); log('fast path: known ' + known.length + ' file(s)') }
const ev = EVAL ? await runChild('code-evaluator', { base: a.base, skill: a.skill, scenarios: a.scenarios, seeds: a.seeds, conditions: a.conditions, steps: stepsFor({ run: 'run', judge: 'judge' }), profile: a.profile, provider: a.provider }) : null
if (EVAL) decisions.push('analytical: code-evaluator ' + (ev && ev.exit !== 'error' ? 'scored ' + ((ev.cells || []).length) + ' cell(s)' + (ev.exit === 'review' ? ' (a run failed: review)' : '') : 'failed: ' + ((ev && ev.error) || 'returned nothing')))
const scanned = SCAN ? await scanFirst() : null
if (scanned && scanned.error) return ERR(scanned.error, tier, route, decisions)
// a blocked/incomplete/stalled/capped scan never reads as clean, even with nothing to fix
const SCAN_EXIT = scanned && KEEP.includes(scanned.exit) ? scanned.exit : ''
if (scanned && (!scanned.known.length || a.fix === false)) return await record(withEval(await sharedGate({ exit: SCAN_EXIT || (scanned.known.length ? 'review' : 'green'), tier, route, packages: 0, merged: scanned.confirmed.length, confirmed: scanned.confirmed, dropped: [], unverified: scanned.unverified, fixed: [], red: [], files: [], decisions, report: head + '\n' + scanned.report })))
const jobs = splitJobs(scanned)
if (!jobs.length) return ERR('nothing to run: no package or path to work on (sizing returned none); pass args.paths or args.packages', tier, route, decisions)
if (GATE && a.fix === false) decisions.push('shared gate: fix=false, no code-looper child; checked once (no edits) at the end')
let gk = null
if (GATE) {
  const clash = GATE_SCOPE.filter((d) => jobs.some((j) => j.group.some((g) => overlaps(d, g))))
  if (clash.length) return ERR('args.gateScope ' + clash.join(', ') + ' overlaps a package group; the layers must be file-disjoint', tier, route, decisions)
  if (a.fix !== false) gk = { child: 'code-looper', group: GATE_SCOPE, args: { cwd: CWD, gate: GATE, scope: GATE_SCOPE, steps: CHILD_STEPS && CHILD_STEPS.fix ? { fix: CHILD_STEPS.fix } : undefined, profile: a.profile, provider: a.provider } }
}
// one code-workers child per top-level dir, CAP per wave; the shared-gate child runs beside the whole wave sequence
const waves = Array.from({ length: Math.ceil(jobs.length / CAP) }, (_, i) => jobs.slice(i * CAP, (i + 1) * CAP))
decisions.push('fan-out: ' + jobs.length + ' code-workers child(ren) in ' + waves.length + ' wave(s) of ≤' + CAP + (PROFILES.length ? ', profiles round-robin ' + PROFILES.join('/') : '') + (gk ? ' ‖ code-looper [' + GATE_SCOPE.join(', ') + ']' : ''))
const [gkRun, ranW] = await parallel([async () => (gk ? runChild(gk.child, gk.args) : null), () => runWaves(waves)])
const ran = ranW || { runs: [], seamFiles: [] }
let res = merge([...jobs.map((j, i) => ({ ...j, r: ran.runs[i] })), ...(gk ? [{ ...gk, r: gkRun }] : [])])
if (res.exit === 'error') return res
if (ran.seamFiles.length) res = { ...res, files: [...new Set([...res.files, ...ran.seamFiles])] }
if (a.fix !== false && res.foreign.length) res = await repairForeign(res)
if (a.fix !== false && a.loop !== false) res = await loopIfNeeded(res, jobs.length, ran.seamFiles)
const pfiles = [...new Set([...touchedFiles(res.fixed), ...res.files])].sort()
const POLISH = a.polish !== false && a.fix !== false
if (POLISH && pfiles.length && lowBudget()) res.decisions.push('polish skipped: budget under 150k tokens remaining')
else if (POLISH && pfiles.length) res = await polishInto(res, pfiles)
res = measured(await sharedGate(res))
if (SCAN_EXIT && res.exit === 'green') res = { ...res, exit: SCAN_EXIT, decisions: [...res.decisions, 'scan exit ' + SCAN_EXIT + ': not green'] }
if (IMPL_EMPTY && res.exit === 'green') res = { ...res, exit: 'incomplete', decisions: [...res.decisions, 'implementer changed no file: the requested change was not made, exit incomplete'] }
if (IMPL_NO_LIVE && res.exit === 'green') res = { ...res, exit: 'review', decisions: [...res.decisions, 'implementer reported no live run: exit review'] }
return await record(withEval(res))

// polishInto: the polish over the merged diff's files (stagePolish); its red packages and decisions merge into r0.
async function polishInto(r0, files) {
  // the polish prompts all build on ctx.rules (the stage block is copied verbatim), so the protected task rides there
  const keep = TASK ? ' Protected requirement: the task below is the owner\'s requested change; never revert, inline away, rename back or otherwise undo what it asks for; flag instead.\nTask: ' + TASK + '\n' : ''
  const po = await stagePolish({ ...CTX, rules: CTX.rules + keep }, { files })
  const fs = po.review.findings
  const sev = ['critical', 'high', 'medium', 'low'].map((k) => [k, fs.filter((f) => f.severity === k).length]).filter((x) => x[1])
  const tally = 'review: ' + fs.length + ' finding(s) (' + (sev.map((x) => x[1] + ' ' + x[0]).join(', ') || 'none') + ')'
  return {
    ...r0, polish: po, exit: po.red.length ? 'red' : r0.exit, red: [...new Set([...r0.red, ...po.red])], files: [...new Set([...r0.files, ...po.files])],
    decisions: [...r0.decisions, ...po.decisions.map((l) => (po.review.ran && /^review: \d/.test(l) ? tally : l))], report: r0.report + po.report,
  }
}

// ── plan route: `agents dispatch --waves` launches, checks and gates; this script starts, awaits and judges ──
// planRun: one runner leaf runs one command (argv, shell-quoted by the leaf) and returns its exit and stdout.
async function planRun(label, argv, note) {
  const r = await agent(
    S.runner.task(RULES + ' Run exactly the argv below once from ' + CWD + ', shell-quoting every element and appending </dev/null; it is one of `claude-code agents dispatch --waves <plan> ...`, ' +
      '`claude-code agents wait <id> --timeout 10m` or `claude-code agents result <id> --verdict --lines 40`. Edit nothing, add no other flag or pipe.' + (note || '') +
      ' Return its exit code as `exit` (null when it could not run) and its stdout as `out` (the last 60 lines when longer).\nargv: ' + JSON.stringify(argv)),
    { phase: 'Run', label: 'plan:' + label, schema: S.runner.schema(PLAN_OUT), model: S.runner.model, effort: S.runner.effort, ...S.runner.type },
  ).catch((e) => { log('plan:' + label + ' failed: ' + ((e && e.message) || e)); return null })
  return S.runner.done('plan:' + label, r)
}
function planJson(r) {
  const m = /\{[\s\S]*\}/.exec((r && r.out) || '')
  try { return m ? JSON.parse(m[0]) : null } catch (e) { return null }
}
// planLaunch: the runner in the background; profile, provider, overlay and a shared gate reach every leaf of the plan.
function planLaunch() {
  return planRun('launch', ['claude-code', 'agents', 'dispatch', '--waves', PLAN_FILE, '--background', '--format', 'json',
    ...(a.provider ? ['--provider', String(a.provider)] : []), ...(a.profile ? ['--profile', String(a.profile)] : PROFILES.length ? ['--profiles', PROFILES.join(',')] : []),
    ...(a.overlay ? ['--overlay', a.overlay] : []), ...(PKG_GATE ? ['--gate', PKG_GATE] : [])])
}
// planAwait: 10-minute waits, a.waitRounds times; 0 or 1 once the runner finished, null while it still runs.
async function planAwait(id) {
  for (let i = 1; i <= (a.waitRounds || 6); i++) {
    const w = await planRun('wait:' + i, ['claude-code', 'agents', 'wait', id, '--timeout', '10m'], ' Set the Bash timeout parameter to its maximum (600000 ms); a call the tool kills counts as exit 4 (still running).')
    if (w && (w.exit === 0 || w.exit === 1)) return w.exit
    log('plan runner ' + id + ' still running after wait ' + i)
  }
  return null
}
// waveGroups: the runner's lines split at each `wave N` header (8 lines max each); lines before the first are `lead`.
function waveGroups(out) {
  const lead = []
  const gs = []
  for (const l of String(out || '').split('\n')) if (l.trim()) { if (/^wave \d+/i.test(l)) gs.push([l]); else (gs.length ? gs[gs.length - 1] : lead).push(l) }
  return { lead, waves: gs.map((g) => g.slice(0, 8).join('\n')) }
}
// planChanged: files changed under the leaves' owned paths, and under those of a gate-failed wave (null: unmeasured).
async function planChanged() {
  const waves = (planJson(await planRun('status', ['claude-code', 'agents', 'dispatch', '--waves', PLAN_FILE, '--status', '--format', 'json'])) || {}).waves || []
  const filesOf = async (ws, label) => {
    const owns = [...new Set(ws.flatMap((w) => (w.leaves || []).flatMap((l) => strs(l.owns))))]
    const m = owns.length ? await stSnap(CTX, label, owns) : null
    return m ? Object.keys(m).sort() : null
  }
  return { files: await filesOf(waves, 'plan'), gateFiles: (await filesOf(waves.filter((w) => w.status === 'failed' && /gate/.test(w.reason || '')), 'plan:gate')) || [] }
}
// planLoop: code-looper over a gate-failed wave's dirs; the plan is still unfinished, so its green is only review.
async function planLoop(r0, files) {
  const scope = [...new Set(files.filter((f) => PKG_GATE || /\.go$/.test(f)).map((f) => stDir(stRel(CTX, f))))].sort()
  if (!scope.length) return r0
  const lp = await loopAfter(r0, scope, 'the wave gate failed', ': rerun the plan command to resume')
  return { ...r0, exit: lp.green ? 'review' : 'red', red: lp.green ? r0.red : scope, files: lp.files, decisions: lp.decisions }
}

// planOut: the plan route's result shape; o overrides.
function planOut(o) { return { tier, route, packages: 0, merged: 0, confirmed: [], dropped: [], unverified: [], fixed: [], red: [], files: [], waves: [], decisions, report: head + '\n\nPlan: ' + PLAN_FILE, ...o } }
async function planVerdict(id, done) { // the runner's verdict as a plan result, incomplete while it still runs
  if (done === null) {
    decisions.push('runner still running after ' + (a.waitRounds || 6) + ' wait(s): claude-code agents wait ' + id + ', then claude-code agents result ' + id + ' --verdict')
    return planOut({ exit: 'incomplete', id })
  }
  const r = await planRun('result', ['claude-code', 'agents', 'result', id, '--verdict', '--lines', '40'], ' Also return `runner`: the runner exit code (0-3) the printed text states, null when it states none.')
  const code = done === 0 ? 0 : Number.isInteger(r && r.runner) ? r.runner : null
  const exit = PLAN_EXIT[code] || 'incomplete'
  decisions.push('runner ' + id + ' exited ' + (code === null ? 'unknown' : code) + ' → ' + exit)
  const { lead, waves } = waveGroups(r && r.out)
  if (lead.length) decisions.push('runner verdict: ' + lead.join(' | '))
  return planOut({ exit, id, waves, report: head + '\n\nPlan: ' + PLAN_FILE + '\n\n' + waves.join('\n\n') })
}
async function planFinish(r0) { // changed files of a finished plan, then the wave-gate loop (red), the polish (green, polish:true)
  const ch = await planChanged()
  if (!ch.files) decisions.push('changed files unmeasured: the status read or the numstat reader returned nothing')
  let res = { ...r0, files: ch.files || [] }
  if (res.exit === 'red' && ch.gateFiles.length) res = await planLoop(res, ch.gateFiles)
  if (res.exit === 'green' && a.polish === true && res.files.length && !lowBudget()) res = await polishInto(res, res.files)
  return res
}
// planRoute: start the plan in the background, wait for it, then loop the red dirs, polish (polish:true) and record.
async function planRoute() {
  if (a.dryPlan) {
    const d = await planRun('dry', ['claude-code', 'agents', 'dispatch', '--waves', PLAN_FILE, '--dry-run'])
    if (!d || d.exit !== 0) return ERR('plan dry run failed: ' + String((d && d.out) || 'runner leaf returned nothing').split('\n')[0], tier, route, decisions)
    decisions.push('dry plan: the packages are listed, nothing launched')
    return planOut({ exit: 'plan', waves: waveGroups(d.out).waves })
  }
  const launch = planJson(await planLaunch())
  const id = launch && typeof launch.id === 'string' ? launch.id : ''
  if (!id) return ERR('the runner leaf started no plan run (no id printed)', tier, route, decisions)
  decisions.push('plan runner ' + id + ' started in the background' + (launch.log ? ' (log ' + launch.log + ')' : ''))
  const res = await planVerdict(id, await planAwait(id))
  return await record(res.exit === 'incomplete' || res.exit === 'error' ? res : await planFinish(res))
}
// runWaves runs the children CAP at a time; after each wave one serial seam step applies what its fixers could not.
async function runWaves(ws) {
  const runs = []
  const seamFiles = []
  for (const [w, js] of ws.entries()) {
    const rs = await parallel(js.map((j) => () => runChild(j.child, j.args)))
    runs.push(...rs)
    const need = rs.flatMap((r) => (r && Array.isArray(r.seams) ? r.seams : []))
    if (need.length && a.fix !== false) seamFiles.push(...(await applySeams(need, w + 1)))
  }
  return { runs, seamFiles }
}
// applySeams: exported-signature and cross-package changes the children reported, applied once with every caller (nothing else runs).
async function applySeams(need, w) {
  phase('Seam')
  const sf = [...new Set(need.map((x) => stRel(CTX, x.file)).filter(Boolean))]
  const snap = (label) => (sf.length ? stSnap(CTX, label, sf) : null) // no path would diff the whole tree
  const pre = await snap('pre:seam:' + w)
  const r = await stageAsk(CTX, CS.fix, 'seam:' + w, 'Seam', RULES + ' Seam step after wave ' + w + ': the package fixers below left these changes undone because each changes an exported signature or a file outside its package. ' +
    'Apply them now and update every caller (grep the whole repo); smallest correct change; gofmt and go vet what you touched. Return `files`.\n' + need.map((x) => '- ' + x.pkg + ': ' + x.file + ' — ' + x.need).join('\n'), ST_WORK)
  stDelta('seam', stDir, pre, await snap('seam:' + w))
  decisions.push('seam step after wave ' + w + ': ' + need.length + ' need(s), ' + (r ? r.status : 'agent returned nothing'))
  return r ? strs(r.files).map((f) => stRel(CTX, f)) : []
}
// measured: red = every dir whose last gate is not green (self-reported reds included unless a later gate proved them green).
function measured(r0) {
  const touched = [...touchedFiles(r0.fixed), ...strs(r0.files)].filter((f) => PKG_GATE || /\.go$/.test(f)).map((f) => stDir(stRel(CTX, f)))
  const red = [...new Set([...stGateRed([...ST_GATES.map((g) => g.dir), ...touched, ...strs(r0.red)]), ...((r0.polish && r0.polish.hold) || [])])].sort()
  const d = [...r0.decisions]
  if (red.join() !== [...strs(r0.red)].sort().join()) d.push('exit from the last gate per dir: red ' + (red.join(', ') || 'none') + ' (stage reports: ' + (strs(r0.red).join(', ') || 'none') + ')')
  let exit = red.length ? 'red' : r0.exit === 'red' ? 'green' : r0.exit
  if (exit === 'green' && ST_OVER.length) { exit = 'review'; d.push('over line budget ' + LINE_BUDGET + ': ' + ST_OVER.map((o) => o.pkg + ' net ' + o.net).join(', ') + ' — exit review') }
  return { ...r0, exit, red, decisions: d }
}
// absorb folds a child's measured state (numstat rows, gate results, proposed tests, over-budget fixes) into this run's, in order.
function absorb(r) {
  if (!r) return
  ST_LINES.push(...((r.lines && r.lines.stages) || []))
  ST_GATES.push(...(Array.isArray(r.gates) ? r.gates : []))
  for (const p of Array.isArray(r.proposedTests) ? r.proposedTests : []) if (!ST_PROPOSED.some((q) => q.pkg === p.pkg && q.case === p.case)) ST_PROPOSED.push(p)
  ST_OVER.push(...(Array.isArray(r.over) ? r.over : []))
}

// implOver: [pkg, net] for each package whose measured impl rows (shrink pass included) are over the line budget.
function implOver() { const m = {}; for (const r of ST_LINES.filter((x) => x.measured && /^impl/.test(x.stage))) m[r.pkg] = (m[r.pkg] || 0) + r.prod.net; return Object.entries(m).filter(([, n]) => stOver(CTX, n)) }
async function implShrink(dirs, snap) { // ONE shrink-or-justify pass over an over-budget implementer; returns the new snapshot
  const why = S.impl.done('impl:shrink', await agent(S.impl.task(RULES + ' Line budget ' + LINE_BUDGET + ' (production net ≤ ' + ST_BUDGET[LINE_BUDGET] + ' per package, git diff --numstat): your change measured ' +
    implOver().map(([p, n]) => p + ' ' + n).join(', ') + '. Shrink it or justify it: cut what the task does not need and keep the change; never revert it, never git checkout/restore/stash; cannot shrink: edit nothing, reason in summary. Return `files`.\nTask: ' + TASK),
    { phase: 'Route', label: 'impl:shrink', schema: S.impl.schema(ST_WORK), model: S.impl.model, effort: S.impl.effort, ...S.impl.type }).catch(() => null))
  const s2 = await stSnap(CTX, 'impl:shrink', dirs)
  stDelta('impl:shrink', stDir, snap, s2)
  if (why) IMPL_FILES = [...new Set([...IMPL_FILES, ...strs(why.files).map((f) => stRel(CTX, f))])]
  for (const [pkg, net] of implOver()) ST_OVER.push({ pkg, net, budget: LINE_BUDGET, why: (why && why.summary) || '' })
  return s2
}
async function implTest(dirs, snap) { // kind feature earns ONE new test; every other test is proposed in the report
  const t = await stageAsk(CTX, CS.test, 'test:feature', 'Test', RULES + ' A feature just landed (' + stDiff(dirs) + '; changed: ' + IMPL_FILES.join(', ') + '). Task: ' + TASK +
    '\nWrite ONE test for the feature\'s main behavior that calls the real code, and repair any test the change broke. Edit only *_test.go and testdata files in ' + dirs.join(', ') + '.' + ST_PROPOSE + ' Put "n/a" in callers.', ST_TESTW)
  for (const p of (t && t.proposed) || []) ST_PROPOSED.push({ pkg: dirs.join(','), ...p })
  if (t) IMPL_FILES = [...new Set([...IMPL_FILES, ...strs(t.files).map((f) => stRel(CTX, f))])]
  stDelta('test:feature', stDir, snap, t ? await stSnap(CTX, 'test:feature', dirs) : snap)
  decisions.push('feature test: ' + (t ? t.status + ' — ' + t.summary : 'agent returned nothing'))
}
// implMeasured: the implementer's numstat per package, a shrink pass when one is over budget, then a kind-feature test.
async function implMeasured(pre, dirs, impl) {
  let snap = await stSnap(CTX, 'impl', dirs)
  const d = stDelta('impl', stDir, pre, snap)
  if (impl && d.measured && implOver().length) snap = await implShrink(dirs, snap)
  if (KIND === 'feature' && impl && impl.status === 'done' && IMPL_FILES.length) await implTest(dirs, snap)
}
// sharedGate: one check-only re-run of the shared gate after every child and the polish (either may change it).
async function sharedGate(r0) {
  if (!GATE) return r0
  const sg = await stageLoop(CTX, { gate: GATE, scope: GATE_SCOPE, check: CS.gate, max: 0, label: 'shared-gate' })
  const d = [...r0.decisions, 'shared gate re-check after all children and polish: ' + (sg.exit === 'green' ? 'green' : 'red: ' + texts(sg.residual, 1).join('; '))]
  return sg.exit === 'green' ? { ...r0, decisions: d } : { ...r0, exit: 'red', red: [...new Set([...r0.red, ...GATE_SCOPE])], decisions: d }
}

function overlaps(x, y) { return x === y || x.startsWith(y + '/') || y.startsWith(x + '/') }
// splitJobs: one code-workers child per top-level dir (a root package: one child for all); groups never share a tree.
function splitJobs(sc) {
  // every dir the scan named: code-workers applies known only to the exact packages it is given, never to nested ones
  const exact = sc ? [...new Set(sc.known.map((x) => stDir(stRel(CTX, x.file))))].sort() : null
  const scanDirs = exact ? topDirs(exact) : null
  const tops = (scanDirs || topDirs(target.packages || dirsOf(target.paths))).sort()
  const kn = sc ? sc.known : known
  const groups = tops.some((d) => d === '' || d === '.') ? [tops] : tops.map((d) => [d]) // a root package's gate spans every dir
  return groups.map((g, i) => {
    const mine = (f) => g.some((d) => overlaps(dirsOf([f])[0], d))
    const k = kn.filter((x) => mine(x.file))
    const t = exact ? { packages: exact.filter((x) => g.some((top) => overlaps(x, top))) } : target.packages ? { packages: g } : { paths: target.paths.filter(mine) }
    return { child: 'code-workers', group: g, args: { cwd: CWD, ...t, fix: a.fix, minFix: a.minFix, focus, steps: CHILD_STEPS, profile: profileOf(i), provider: a.provider, polish: false, gate: PKG_GATE,
      kind: KIND || undefined, lineBudget: LINE_BUDGET, ...(k.length ? { known: k } : {}) } }
  })
}

function mergeBad(r) { return !r || r.exit === 'error' }
function mergeCat(ok, k) { return ok.flatMap((x) => lst(x.r[k])) }
function mergeState(done, code, ok) { // decisions, red dirs, files and the non-green child exits (worst first)
  const tag = (x, d) => (code.length > 1 ? '[' + x.group.join(', ') + '] ' + d : d)
  const d = [...decisions, ...ok.flatMap((x) => lst(x.r.decisions).map((l) => tag(x, l)))]
  const red = mergeCat(ok, 'red')
  for (const x of code.filter((y) => mergeBad(y.r))) { d.push(tag(x, x.child + ' failed: ' + errOf(x.r, 'returned nothing'))); red.push(...x.group) }
  const files = mergeCat(ok, 'files')
  const gk = done.find((x) => x.child === 'code-looper')
  if (gk) {
    const g = gk.r || { exit: 'error', iterations: 0, filesChanged: [], residual: [{ text: 'code-looper returned nothing' }] }
    d.push('shared gate ' + g.exit + (g.iterations ? ' after ' + g.iterations + ' fix iteration(s)' : '') + ' over ' + gk.group.join(', ') + (g.exit === 'green' ? '' : ': ' + texts(g.residual, 3).join('; ')))
    if (g.exit !== 'green') red.push(...gk.group)
    files.push(...strs(g.filesChanged))
  }
  // a child that ended blocked/stalled/capped/incomplete (red with no red list included) never merges to green
  const kept = ok.filter((x) => KEEP.includes(x.r.exit)).sort((x, y) => KEEP.indexOf(x.r.exit) - KEEP.indexOf(y.r.exit))
  for (const x of kept) d.push(tag(x, x.child + ' exit ' + x.r.exit + ': not green'))
  for (const x of ok.filter((y) => y.r.exit === 'red' && !strs(y.r.red).length)) red.push(...x.group)
  return { d, red: [...new Set(red)], files, kept }
}
function mergeDups(ok) { // a finding cited outside its group that the owning group confirmed is one bug: drop the copy
  const owns = (x, f) => x.group.some((dd) => overlaps(dirsOf([stRel(CTX, f.file)])[0], dd))
  return (x, f) => !owns(x, f) && ok.some((o) => o !== x && owns(o, f) && lst(o.r.confirmed).some((g) => sameBug(CTX, g, f)))
}
// merge folds the children's results into one; a failed child turns its group red, and only an all-failed run is an error.
function merge(done) {
  for (const x of done) absorb(x.r)
  const code = done.filter((x) => x.child !== 'code-looper')
  if (code.every((x) => mergeBad(x.r))) return ERR(errOf(code[0].r, code[0].child + ' returned nothing'), tier, route, decisions)
  const ok = code.filter((x) => !mergeBad(x.r))
  const { d, red, files, kept } = mergeState(done, code, ok)
  const dupOwned = mergeDups(ok)
  const confirmed = ok.flatMap((x) => lst(x.r.confirmed).filter((f) => !dupOwned(x, f))).sort((x, y) => ORDER[x.severity] - ORDER[y.severity])
  return {
    exit: red.length ? 'red' : kept.length ? kept[0].r.exit : a.fix === false ? 'review' : 'green', tier, route, packages: ok.reduce((n, x) => n + (x.r.packages || 0), 0), merged: ok.reduce((n, x) => n + (x.r.merged || 0), 0),
    confirmed, dropped: mergeCat(ok, 'dropped'), unverified: mergeCat(ok, 'unverified'), fixed: mergeCat(ok, 'fixed'), foreign: ok.flatMap((x) => lst(x.r.foreign).filter((f) => !dupOwned(x, f))), red, files: [...new Set(files.map((f) => stRel(CTX, f)))],
    decisions: d, report: head + '\n' + ok.map((x) => x.r.report || '').join('\n\n'),
  }
}

// scanFirst: code-analyzer audits the target review-only; its confirmed findings become code-workers' known bugs (Find and Rederive skipped).
async function scanFirst() {
  const where = ANALYZE ? { range: RANGE } : { paths: target.packages || target.paths }
  const sc = await runChild('code-analyzer', { cwd: CWD, ...where, fix: false, minFix: MIN_FIX, focus, dimensions: a.dimensions, kind: KIND || undefined, steps: stepsFor({ find: 'audit', rederive: 'refute' }), profile: a.profile, provider: a.provider })
  if (!sc || sc.exit === 'error') return { error: 'code-analyzer scan failed: ' + errOf(sc, 'returned nothing') }
  const confirmed = lst(sc.confirmed)
  const known = confirmed.map((f) => ({ file: stRel(CTX, f.file), line: f.line, symbol: f.symbol, severity: f.severity, kind: f.kind, symptom: f.title || f.symptom, scenario: f.scenario, evidence: f.evidence }))
  decisions.push('analytical: code-analyzer scan exit ' + sc.exit + ', ' + confirmed.length + ' confirmed, ' + lst(sc.refuted).length + ' refuted → ' + (known.length ? 'code-workers fix them' : 'nothing to fix'))
  const noLine = confirmed.filter((f) => !f.line).length
  if (noLine) decisions.push('analytical: ' + noLine + ' handed-over finding(s) carry no line')
  return { exit: sc.exit, known, confirmed, unverified: lst(sc.unverified), report: sc.report || '' }
}

async function loopAfter(r0, scope, why, ok, more) { // code-looper over scope (more = extra child args): green, merged files and the decision line
  const gk = await runChild('code-looper', { cwd: CWD, scope, gate: PKG_GATE, ...more, steps: stepsFor({ fix: 'fix' }), profile: a.profile, provider: a.provider })
  absorb(gk)
  const green = !!gk && gk.exit === 'green'
  return { green, files: [...new Set([...r0.files, ...strs(gk && gk.filesChanged).map((f) => stRel(CTX, f))])],
    decisions: [...r0.decisions, 'loop (' + why + '): code-looper over ' + scope.join(', ') + ' exit ' + ((gk && gk.exit) || 'none') +
      (green ? ok : ': ' + [errOf(gk, ''), ...texts(gk && gk.residual, 2)].filter(Boolean).join('; '))] }
}
// loopIfNeeded: code-looper over the dirs whose fix landed but gate stayed red, and over every touched dir when 2+ children ran.
async function loopIfNeeded(r0, nChildren, extra) {
  const gateRed = [...new Set(lst(r0.fixed).filter((f) => f.impl && f.impl.status === 'done' && f.gate && f.gate.exit !== 'green').map((f) => f.pkg))]
  const touched = [...new Set([...touchedFiles(r0.fixed), ...r0.files, ...extra].filter((f) => PKG_GATE || /\.go$/.test(f)).map((f) => stDir(stRel(CTX, f))))]
  // a touched dir no green gate covers yet (implementer, seam, foreign edits) is gated here: the exit reads gates only
  const scope = [...new Set([...gateRed, ...(nChildren > 1 ? touched : []), ...stGateRed(touched)])].filter((d) => !GATE_SCOPE.some((g) => overlaps(d, g))).sort()
  if (!scope.length) return r0
  const spent = Math.max(0, ...lst(r0.fixed).filter((f) => gateRed.includes(f.pkg)).map((f) => (f.gate && f.gate.iterationsUsed) || 0))
  const lp = await loopAfter(r0, scope, gateRed.length ? 'gate red in ' + gateRed.join(', ') : nChildren + ' children shared the run', '', { iterationsUsed: spent })
  const red = lp.green ? r0.red.filter((d) => !gateRed.includes(d)) : [...new Set([...r0.red, ...scope])]
  return { ...r0, exit: red.length ? 'red' : r0.exit === 'red' ? 'green' : r0.exit, red, files: lp.files, decisions: lp.decisions }
}
// withEval attaches the analytical code-evaluator result (it ran before any code changed).
function withEval(r) { return ev ? { ...r, runs: ev.runs, cells: ev.cells || [] } : r }
// stepsFor renames steps.child's stages into another child's stage names (map: ours → theirs), dropping the rest.
function stepsFor(map) {
  const out = {}
  for (const [k, v] of Object.entries(CHILD_STEPS || {})) if (map[k]) out[map[k]] = v
  return Object.keys(out).length ? out : undefined
}
// repairForeign fixes the findings children cited outside their groups, one dir at a time after every child finished (one owner per file).
async function repairForeign(r0) {
  const byDir = {}
  for (const f of r0.foreign) (byDir[stDir(stRel(CTX, f.file))] = byDir[stDir(stRel(CTX, f.file))] || []).push(f)
  const fixed = []
  const d = []
  const red = []
  for (const dir of Object.keys(byDir)) {
    // a Go gate over a dir with no Go files can only go red: non-Go findings get the fix alone, unless args.gate is set
    const goDir = !!PKG_GATE || byDir[dir].some((f) => /\.go$/.test(String(f.file)))
    const fx = goDir ? await stageRepair(CTX, dir, byDir[dir], { label: 'foreign:' + dir, gate: 'gate:foreign:' + dir })
      : { ...(await stageFix(CTX, { pkg: dir, findings: byDir[dir], tests: false, label: 'foreign:' + dir })), gate: null, green: undefined }
    if (!goDir) fx.green = fx.done
    fixed.push({ pkg: dir, findings: byDir[dir].length, impl: fx.impl, test: fx.test, gate: fx.gate, files: fx.files })
    d.push('foreign: ' + byDir[dir].length + ' finding(s) a child cited in ' + dir + ' repaired by code-engineer after the children: ' + (fx.done ? 'fixed' : 'fix NOT done') + ', gate ' + (fx.green ? 'green' : 'red'))
    if (!fx.green) red.push(dir)
  }
  const reds = [...new Set([...r0.red, ...red])]
  return { ...r0, exit: reds.length ? 'red' : r0.exit, red: reds, fixed: [...r0.fixed, ...fixed], files: [...new Set([...r0.files, ...fixed.flatMap((x) => x.files)])], decisions: [...r0.decisions, ...d] }
}
// touchedFiles is the sorted, deduped union of the files the implementer, fixers, gate fixers and test writers reported changing.
function touchedFiles(fixed) {
  const fs = (fixed || []).flatMap((f) => [...strs(f.files), ...strs(f.impl && f.impl.files), ...strs(f.test && f.test.files)])
  return [...new Set([...IMPL_FILES, ...fs].map((f) => stRel(CTX, f)))].sort()
}

function recordOnce(res, label) { return agent(
  S.record.task(RULES + ' Record this run and nothing else. Write the JSON below verbatim to a NEW file from `mktemp "${TMPDIR:-/tmp}/record.XXXXXX"` ' +
    '(Read that empty file before you Write it), then run exactly `cd ' + CWD + ' && claude-code memory report --label ' + label + ' --file <that file> --format json </dev/null` ' +
    'and return its paths array. Edit nothing else.\nJSON:\n' + JSON.stringify({ report: res.report, decisions: res.decisions })),
  { phase: 'Record', label: 'record', schema: S.record.schema(RECORDED), model: S.record.model, effort: S.record.effort, ...S.record.type },
).catch(() => null) }
// record appends the run's report and decisions to the repo's memory reports so continuity needs no hand-written notes.
async function record(res0) {
  const files = [...new Set([...touchedFiles(res0.fixed), ...strs(res0.files)])].sort()
  const lines = stLines(ST_LINES)
  const res = { ...res0, files, lines, proposedTests: ST_PROPOSED, over: ST_OVER, gates: ST_GATES,
    report: res0.report + (files.length ? '\n\nFiles touched: ' + files.join(', ') : '') + (ST_LINES.length || ST_PROPOSED.length ? '\n' + stTail(lines, ST_OVER, ST_PROPOSED) : '') }
  if (a.record === false) return { ...res, coworkers: COWORKERS }
  const label = String(a.label || 'code-engineer').replace(/[^A-Za-z0-9._-]+/g, '-')
  let w = S.record.done('record', await recordOnce(res, label))
  if (!w || !lst(w.paths).length) w = S.record.done('record', await recordOnce(res, label)) // a no-op record (paths []) gets one retry
  const out = { ...res, coworkers: COWORKERS, recorded: w ? w.paths : [], decisions: [...res.decisions, w ? 'recorded: ' + w.paths.join(', ') : 'record agent returned nothing'] }
  const path = w && (lst(w.paths).find((p) => /\/reports\//.test(p)) || lst(w.paths)[0])
  return path ? slimmed(out, path) : out
}

// slimmed: a recorded result minus the report text and tables (saved in the report), plus a short verdict and its path.
function slimmed(r, reportPath) {
  const verdict = ['exit ' + r.exit, 'tier ' + r.tier + ' route ' + r.route, 'red ' + (strs(r.red).join(', ') || 'none'),
    'files ' + strs(r.files).length, 'proposed tests ' + ST_PROPOSED.length, 'report ' + reportPath].join('\n')
  return { ...Object.fromEntries(Object.entries(r).filter(([k]) => !['report', 'proposedTests', 'lines', 'over', 'gates'].includes(k))), verdict, reportPath }
}
