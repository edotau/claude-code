export const meta = {
  name: 'code-engineer',
  description: 'One size-routed entry point: analytical first when the task needs it (code-analyzer scans an audit or range, code-evaluator scores a skill), then 1-3 parallel code-workers children fix, green-keeper-loop loops on a red gate or a multi-child run; then one polish (an epic drafts a plan)',
  whenToUse:
    'USE: any code job when you do not want to pick a workflow; sizes it (TRIVIAL/LOW/MEDIUM/HIGH/EPIC) and routes. CALL: Workflow({scriptPath: "~/.claude/workflows/code-engineer.js", args: {cwd, task?, packages?, paths?, range?, skill?}}) with at least one of task/packages/paths/range/skill (range = a git range like main..HEAD; a bare range is scanned by code-analyzer and its findings fixed by code-workers; with a task, paths or packages its changed Go files become paths). OPTIONAL: kind (feature|refactor|fix|audit; wins over the size agent and the task-verb guess; feature and refactor get an implementer, which exits incomplete when it changes no file and review when it reports no live run), tier (skips sizing), fix (default true), minFix (default low: every finding is fixed, decision #54), focus, dimensions (code-analyzer lenses on a scan route), label (report file name, default code-engineer), record (default true; false skips the Record step), steps {size, intel, impl, plan, dedupe, simplify, review, record, child} = {agent?, model?, effort?, profile?, provider?} per stage (agent claude|codex|copilot|gemini|opencode; external stages run on their pins, or args/steps profile|provider, and fall back on 429/auth; each reports ran on or BLOCKED). OPTIONAL: profile, provider (run-wide override for external stages). child = the routed child\'s own steps (simplify/review/dedupe are rejected there: code-engineer polishes once); its fix, test and gate also drive the polish Fix-all. Fan-out: the target\'s top-level dirs split into at most 3 file-disjoint groups, one code-workers child each (polish:false), all launched in parallel. gate (a shared gate command) with gateScope (dirs its fixer may edit, disjoint from the groups): one green-keeper-loop child runs beside them (none under fix:false); the shared gate is re-checked once, check-only, after every child and the polish (a clean scan included). gate without gateScope: every package\'s gate instead (passed to the code-workers children, the loop and code-engineer\'s own repairs; e.g. npm test for JS/TS). Findings a child cites outside its group come back to code-engineer, which repairs them once after the children. skill (+ base, scenarios, seeds, conditions): a skill/route comparison, routed to code-evaluator. polish (default true; false skips it) runs once after the children over the merged diff: Dedupe (2+ packages), Simplify, Review; each package with review findings gets one Fix-all, guard test and gate (no second child run). fast (default true; false disables): a LOW/MEDIUM fix task passes its files to code-workers as known, skipping Find and Rederive. A low token budget (under 150k left) skips the polish. RETURNS {exit (green|red|review|plan|error, or a child\'s blocked|stalled|incomplete|capped), tier, route, packages, merged, confirmed, dropped, unverified, fixed, red, files (sorted repo-relative files the implementer, fixers, gate fixers and polish changed), polish, decisions, report} (runs, cells on the code-evaluator route). Details: saved-workflows.md.',
  phases: [
    { title: 'Size', detail: 'one cheap agent reads the task, git status/diff and go list and returns the tier, packages and paths' },
    { title: 'Route', detail: 'MEDIUM adds an intel agent, a feature task adds an implementer, EPIC drafts a plan and stops' },
    { title: 'Run', detail: 'code-evaluator (skill) and code-analyzer scan (audit, package list, bare range; scan:false skips) first; then 1-3 code-workers children on disjoint dir groups (known = the scan findings); green-keeper-loop when a gate stays red or 2+ children ran (loop:false skips)' },
    { title: 'Dedupe', detail: 'once over the merged diff, when 2+ packages changed (polish:false skips)' },
    { title: 'Simplify', detail: 'once over the merged diff; review simplify plus the simplify gate' },
    { title: 'Review', detail: 'once over the merged diff; review findings get one Fix-all, guard test and gate per package (no second child run)' },
    { title: 'Fix', detail: 'Fix-all: one implementer per package with review findings' },
    { title: 'Test', detail: 'one guard test per Fix-all change, proven with claude-code test mutate' },
    { title: 'Gate', detail: 'package-scoped re-gate after Fix-all, up to 2 fixer iterations on red' },
    { title: 'Record', detail: 'one cheap agent appends the report and decisions to the memory store (record:false skips)' },
  ],
}

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
// Shared stages, run in-script so workflow() nests one level; ctx = {cwd, cd, rules, S, model?, gateMax?, gate?}.
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
  const out = (status, exit, hist, residual) => {
    if (status !== 'green' && hist.length) log(tag + ' still ' + status + ' after ' + hist.length + ' iteration(s); residual: ' + residual.length)
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
// stageFix: one implementer over confirmed findings in one package, then one guard test per behavior change.
async function stageFix(ctx, o) {
  const item = (f) => '- ' + stRel(ctx, f.file) + ':' + (f.line || 0) + ' (' + (f.symbol || '?') + ', ' + f.severity + (stCost(f) ? ', ' + f.kind : '') + '): ' + (f.symptom || f.title) +
    (f.scenario ? '\n  scenario: ' + f.scenario : '') + (f.evidence ? '\n  evidence: ' + f.evidence : '')
  const list = o.findings.map(item).join('\n')
  const bugs = o.findings.filter((f) => !stCost(f))
  const costs = o.findings.filter(stCost)
  const own = o.owns && o.owns.length ? ' You own ONLY: ' + o.owns.join(', ') + '; list any other file you would need in leftUndone.'
    : ' Edit only non-test files under ' + o.pkg + ', except a *_test.go file a finding below cites.'
  const impl = await stageAsk(ctx, ctx.S.fix, 'fix:' + o.label, 'Fix', ctx.rules + ' Fix these confirmed defects in ' + o.pkg + ' and nothing else.' + own +
    ' The evidence is verified: do not re-audit; first tool call: cat the cited files. The Test stage writes any requested test; that is not unfinished work, so report done. ' +
    'Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 4-6). Root cause first, smallest correct change, functions < 50 lines, 1-line comments. ' +
    'Grep every caller outside the package of each function you changed and report in `callers` whether any now duplicates or contradicts the new behavior (do not edit them). ' +
    'Run the repo formatter and linter on what you touched' + (o.owns ? '' : ' (gofmt + go vet ./' + o.pkg + '/... for Go)') + '. Return `files`: every repo-relative file you changed.\n' + list, ST_WORK, o.pin)
  const done = !!(impl && impl.status === 'done')
  const tdirs = [...new Set([o.pkg, ...((impl && impl.files) || []).map((f) => stDir(stRel(ctx, f))).filter((d) => d === o.pkg || d.startsWith(o.pkg + '/'))])]
  const test = done && o.tests !== false ? await stageAsk(ctx, ctx.S.test, 'test:' + o.label, 'Test', ctx.rules + ' Fixes just landed in ' + o.pkg + ' (' + stDiff([o.pkg]) + '; changed: ' + (impl.files || []).join(', ') + '). ' +
    'Repair tests the fixes break. Edit only *_test.go and testdata files directly in ' + tdirs.join(', ') + '; never another package\'s tests or shared fixtures (list any you would need in leftUndone). Each test under complexity 10. Skip comment-only and dead-code fixes. Put "n/a" in callers.' +
    (bugs.length ? '\nWrite ONE regression test per bug fix that calls the real code and fails without the fix; for Go prove each with `claude-code test mutate <file> <old> <new> --run <TestName> ./' + o.pkg + ' </dev/null` (exit 0 = caught; a bare test name and no globs: copilot\'s shell refuses `$`, `|` and `*` even inside quotes).\n' + bugs.map(item).join('\n') : '') +
    (costs.length ? '\nFor each cost fix, pin the unchanged behavior it must keep: a test that calls the real code and passes before and after the fix ' +
      '(it need not fail without it; no mutate). When existing tests already pin that behavior, name them in summary and add none.\n' + costs.map(item).join('\n') : ''), ST_WORK) : null
  const files = [...new Set([...((impl && impl.files) || []), ...((test && test.files) || [])].map((f) => stRel(ctx, f)))]
  return { pkg: o.pkg, impl, test, done, testOk: o.tests === false || !!(test && test.status === 'done'), files }
}
// stageRepair: stageFix, then stageGate (ctx.gate, else a Go gate) once the fix is done; unchanged code proves nothing.
async function stageRepair(ctx, pkg, findings, o) {
  const fx = await stageFix(ctx, { pkg, findings, label: o.label })
  const gate = fx.done ? await stageGate(ctx, { scope: [o.scope || pkg], check: ctx.S.gate, fix: ctx.S.fix, max: ctx.gateMax || 0, schemas: true, label: o.gate, gate: ctx.gate }) : null
  return { ...fx, gate, green: fx.done && fx.testOk && !!gate && gate.exit === 'green', files: [...fx.files, ...(gate ? gate.filesChanged : [])] }
}
// stagePolish: once over a merged diff: Dedupe (2+ packages), Simplify, Review, then one stageRepair per package.
async function stagePolish(ctx, o) {
  const files = [...new Set(o.files.map((f) => stRel(ctx, f)))]
  const pkgs = [...new Set(files.map(stDir))]
  const scope = ' Only these files (the tree is shared; never touch or diff others): ' + files.join(' ') + '. Diff: ' + stDiff(files) + '.'
  const cut = ' Name the assumption before every cut; flag instead of cutting when it has no covering test; never cut semantic comments, error context or distinct tests; never fix bugs (flag them). ' +
    'After edits run gofmt, go vet and go test for ' + pkgs.join(', ') + '; if a test regresses, revert your own edit by hand (never git checkout/restore/stash) and flag it. ' +
    'Return applied[] and flagged[] as `file:line · what · assumption`, deltaLines (net changed lines, negative = removed) and status (clean = nothing to do).'
  const sum = (k, r) => k + ': ' + (r ? r.applied.length + ' applied, ' + r.flagged.length + ' flagged, Δ ' + r.deltaLines + ' lines (' + r.status + ')' : 'agent returned nothing')
  const lines = []
  let dedupe = null
  if (pkgs.length >= 2 && ctx.S.dedupe) {
    phase('Dedupe')
    dedupe = await stageAsk(ctx, ctx.S.dedupe, 'dedupe', 'Dedupe', ctx.rules + ' Cross-package dedupe, the reuse angle of ' + ctx.cwd + '/skills/simplify/SKILL.md.' + scope +
      ' Look ONLY for helpers or logic duplicated across these packages (grep siblings, `git log -S`); compare a near-duplicate with its original for behavior the copy lost, then consolidate at the original (callers import it; no new shim).' + cut, ST_SIMP)
    lines.push(sum('dedupe', dedupe))
  } else lines.push('dedupe skipped: ' + pkgs.length + ' package(s)')
  phase('Simplify')
  const simplify = await stageAsk(ctx, ctx.S.simplify, 'simplify', 'Simplify', ctx.rules + ' Simplify gate (' + ctx.cwd + '/skills/simplify/SKILL.md, quality.md -> Simplify), run ONCE over the merged diff.' + scope +
    ' Run `claude-code review simplify ' + files.join(' ') + '`, then one in-context pass (reuse, simplification, efficiency, altitude). Edit only the listed non-test files.' + cut, ST_SIMP)
  lines.push(sum('simplify', simplify))
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
      return { pkg: p, findings: byPkg[p].length, done: r.done, green: r.green, files: r.files }
    }))
    ps.forEach((p, i) => { byKey[p] = rs[i] })
  }
  const fixall = keys.map((p) => byKey[p]).filter(Boolean)
  for (const x of fixall) lines.push('fix-all ' + x.pkg + ': ' + (x.done ? x.findings + ' fixed' : 'NOT done') + ', gate ' + (x.green ? 'green' : 'red'))
  const report = ['', '## Polish', ...lines.map((l) => '- ' + l), ...findings.map((f) => '- **' + f.severity + '** `' + f.file + ':' + f.line + '` (' + f.symbol + ') — ' + f.symptom)].join('\n')
  // a review that never ran proves nothing: every polished package stays red
  const red = [...new Set([...fixall.filter((x) => !x.green).map((x) => x.pkg), ...open.map((f) => stDir(f.file)), ...(rv ? [] : pkgs)])]
  return { dedupe, simplify, review: { findings, ran: !!rv }, fixall, incomplete, green: !red.length, red, files: [...new Set(fixall.flatMap((x) => x.files))], decisions: lines, report }
}
// ── end stages ──

const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const CWD = a.cwd || ''
const TASK = a.task ? String(a.task) : ''
const ERR = (m, tier, route, decisions) => ({ exit: 'error', error: 'code-engineer: ' + m, tier: tier || '', route: route || '', decisions: decisions || [], report: '' })
const strs = (v) => (Array.isArray(v) ? v.filter(Boolean).map(String) : [])
// rels makes every path repo-relative: an absolute and a relative spelling of one dir would become two overlapping children
const rels = (v) => [...new Set(strs(v).map((x) => x.replace(CWD ? CWD + '/' : /^$/, '').replace(/^\.\//, '').replace(/\/+$/, '')).filter(Boolean))]
let PATHS = rels(a.paths)
let PKGS = rels(a.packages)
const TIERS = ['TRIVIAL', 'LOW', 'MEDIUM', 'HIGH', 'EPIC']
const RANGE = a.range ? String(a.range) : ''
const EVAL = !!a.skill
// skill alone is a pure comparison; with code targets, code-evaluator runs first, before any code changes
const EVAL_ONLY = EVAL && !TASK && !PATHS.length && !PKGS.length && !RANGE
if (RANGE && !/^[A-Za-z0-9._\/~^@{}-]+(\.\.\.?[A-Za-z0-9._\/~^@{}-]+)?$/.test(RANGE)) return ERR('args.range must be a git range like main..HEAD')
if (!CWD || !(TASK || PATHS.length || PKGS.length || RANGE || EVAL)) return ERR('args.cwd and one of args.task, args.packages, args.paths, args.range or args.skill are required')
if (a.tier !== undefined && !TIERS.includes(a.tier)) return ERR('args.tier must be ' + TIERS.join('|'))
if (a.kind !== undefined && !['feature', 'refactor', 'fix', 'audit'].includes(a.kind)) return ERR('args.kind must be feature|refactor|fix|audit')
// A bare range (no task, paths or packages) is a diff audit, so code-analyzer owns it (D2).
const ANALYZE = !!RANGE && !TASK && !PATHS.length && !PKGS.length
// args.gate + gateScope: a shared gate a green-keeper-loop child keeps green beside the children, editing gateScope.
// args.gate alone: every package's gate, passed to the code-workers children and used by code-engineer's own repairs.
const GATE_SCOPE = strs(a.gateScope).map((d) => d.replace(/\/+$/, ''))
const GATE = a.gate && GATE_SCOPE.length ? String(a.gate) : ''
const PKG_GATE = a.gate && !GATE_SCOPE.length ? String(a.gate) : undefined
// steps.child is the routed child's own steps; the rest are ours.
const { child: CHILD_RAW, ...OWN } = a.steps && typeof a.steps === 'object' && !Array.isArray(a.steps) ? a.steps : {}
// a fix task never runs impl (code-workers' fix stage does the edit), so steps.impl drives that fix stage unless child.fix is set
const CHILD_STEPS = OWN.impl && !(CHILD_RAW && CHILD_RAW.fix) ? { ...(CHILD_RAW || {}), fix: OWN.impl } : CHILD_RAW
for (const k of ['simplify', 'review', 'dedupe']) if (CHILD_STEPS && CHILD_STEPS[k]) return ERR('steps.child.' + k + ': code-engineer runs polish once; set steps.' + k)
const OPT = { external: true, profile: a.profile, provider: a.provider, allowed: ['size', 'intel', 'impl', 'plan', 'dedupe', 'simplify', 'review', 'record'] }
let S
let CS // the child's fix, test and gate stages, run in-script by the polish Fix-all
try {
  S = {
    size: stepOf(OWN, 'size', { model: 'haiku', effort: 'low' }, OPT),
    intel: stepOf(OWN, 'intel', { model: 'sonnet', effort: 'medium' }, OPT),
    impl: stepOf(OWN, 'impl', { tools: 'edit', model: 'sonnet', effort: 'medium' }, OPT),
    plan: stepOf(OWN, 'plan', { model: 'sonnet', effort: 'medium' }, OPT),
    dedupe: stepOf(OWN, 'dedupe', { tools: 'edit', model: 'sonnet', effort: 'medium' }, OPT),
    simplify: stepOf(OWN, 'simplify', { tools: 'edit', model: 'sonnet', effort: 'medium' }, OPT),
    review: stepOf(OWN, 'review', { model: 'sonnet', effort: 'high' }, OPT),
    record: stepOf(OWN, 'record', { tools: 'edit', model: 'haiku', effort: 'low' }, OPT),
  }
  const CO = { ...OPT, allowed: Object.keys(CHILD_STEPS || {}) }
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
      EXT.map((s) => `claude-code gateway router --agent ${s.agent} --fallback agents${s.profile ? ' --profile ' + s.profile : ''}${s.provider ? ' --provider ' + s.provider : ''} --format json </dev/null`).join('\n'),
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
const CTX = { cwd: CWD, cd: '', rules: RULES, S: { ...CS, dedupe: S.dedupe, simplify: S.simplify, review: S.review }, gateMax: ST_GATE_MAX, gate: PKG_GATE }
const SIZED = {
  type: 'object',
  properties: {
    kind: { type: 'string', enum: ['fix', 'feature', 'refactor', 'audit', 'architecture'] },
    packages: { type: 'array', items: { type: 'string' } }, paths: { type: 'array', items: { type: 'string' } },
    focus: { type: 'string' }, reason: { type: 'string' },
  },
  required: ['kind', 'packages', 'paths', 'focus', 'reason'],
}
// tierOf is the deterministic size rule: the agent only finds files and the job kind, the script picks the tier.
const tierOf = (kind, paths, pkgs) => {
  if (kind === 'architecture') return 'EPIC'
  if (kind === 'audit') return 'HIGH'
  const files = paths.filter((p) => /\.[A-Za-z0-9]+$/.test(p))
  const dirs = new Set([...pkgs, ...files.map((p) => p.split('/').slice(0, -1).join('/'))])
  return tierBy(dirs.size, files.length)
}
// tierBy: HIGH past 3 dirs or 8 files, MEDIUM at 2+ of either, else LOW.
const tierBy = (nDirs, nFiles) => (nDirs > 3 || nFiles > 8 ? 'HIGH' : nDirs >= 2 || nFiles >= 2 ? 'MEDIUM' : 'LOW')
// IMPL is ST_WORK plus `live`: the changed surface run on real input before tests, or why there is none.
const IMPL = { ...ST_WORK, properties: { ...ST_WORK.properties, live: { type: 'object', properties: { command: { type: 'string' }, exit: { type: 'integer' }, output: { type: 'string' }, reason: { type: 'string' } }, required: ['command'] } }, required: [...ST_WORK.required, 'live'] }
const INTEL = { type: 'object', properties: { paths: { type: 'array', items: { type: 'string' } }, focus: { type: 'string' } }, required: ['paths', 'focus'] }
const PLAN = { type: 'object', properties: { plan: { type: 'string' } }, required: ['plan'] }
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
if (ANALYZE || EVAL_ONLY) {
  tier = a.tier || (ANALYZE ? 'HIGH' : 'MEDIUM')
  decisions.push('sized as ' + tier + ': ' + (ANALYZE ? 'a bare range is a diff audit (code-analyzer)' : 'a skill/route comparison (code-evaluator)'))
} else if (!tier && !TASK) {
  tier = PKGS.length ? 'HIGH' : 'LOW'
  decisions.push('sized as ' + tier + ': no task, ' + (PKGS.length ? 'a package list is an audit' : 'only named paths'))
} else if (!tier) {
  const sz = S.size.done('size', await agent(
    S.size.task(RULES + ' Read-only: edit nothing. Do NOT pick a size; find facts. Read the code the task points at and return: kind (fix = repair ' +
      'existing behavior, feature = add behavior, refactor = change structure keeping existing behavior, audit = review packages for bugs, architecture = a multi-session design change); the ' +
      'non-test source FILES that must change (repo-relative, one entry per file, never a directory, tests excluded); the Go package dirs ' +
      'only for an audit; a one-line focus; and a one-line reason. Use `git status --short` and `go list ./...` only as needed.\nTask: ' + TASK +
      (PKGS.length ? '\nGiven packages: ' + PKGS.join(', ') : '') + (PATHS.length ? '\nGiven paths: ' + PATHS.join(', ') : '')),
    { phase: 'Size', label: 'size', schema: S.size.schema(SIZED), model: S.size.model, effort: S.size.effort, ...S.size.type },
  ).catch(() => null))
  if (!sz) return ERR('sizing agent returned nothing; pass args.tier to skip it')
  KIND = a.kind || sz.kind
  if (!PKGS.length) PKGS = rels(sz.packages)
  if (!PATHS.length) PATHS = rels(sz.paths).filter((p) => !/_test\.go$/.test(p))
  tier = tierOf(sz.kind, PATHS, sz.kind === 'audit' ? PKGS : [])
  log('size: kind ' + sz.kind + ', ' + PATHS.length + ' file(s), ' + PKGS.length + ' package(s) -> ' + tier)
  decisions.push('sized as ' + tier + ' (rule: ' + sz.kind + ', ' + PATHS.length + ' file(s)): ' + sz.reason)
  if (!a.focus && sz.focus) focus = sz.focus
} else if (a.tier) decisions.push('sized as ' + tier + ': args.tier given')

// ── Route ──
// analytical first: an audit (bare range, package list with no task, or kind audit) scans with code-analyzer, then code-workers fix what it confirmed
const SCAN = a.scan !== undefined ? !!a.scan : ANALYZE || KIND === 'audit' || (!TASK && PKGS.length > 0)
const route = EVAL_ONLY ? 'code-evaluator' : tier === 'EPIC' ? 'plan' : (EVAL ? 'code-evaluator→' : '') + (SCAN ? 'code-analyzer→' : '') + (tier === 'HIGH' || ANALYZE ? 'code-workers:packages' : 'code-workers:paths')
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
if (EVAL_ONLY) {
  const ev = await runChild('code-evaluator', { base: a.base, skill: a.skill, scenarios: a.scenarios, seeds: a.seeds, conditions: a.conditions, steps: CHILD_STEPS, profile: a.profile, provider: a.provider })
  if (!ev || ev.error) return ERR((ev && ev.error) || 'code-evaluator returned nothing', tier, route, decisions)
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
  const impl = S.impl.done('impl', await agent(
    S.impl.task(RULES + ' Make this change in these files (new files in the same package ok) and nothing else; smallest correct change, functions < 50 lines, 1-line comments. ' +
      'Run gofmt and go vet on the package. Do not write tests; code-workers follows with find, fix, guard test and gate. Return `files`: every repo-relative file you changed. ' +
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
}

// ── Run ──
phase('Run')
// Fast path: a LOW/MEDIUM fix task names the bug, so code-workers skips Find and Rederive for those files (audits and features never).
const fastFiles = (tier === 'LOW' || tier === 'MEDIUM') && KIND === 'fix' && TASK && a.fix !== false && a.fast !== false ? (sized.length ? sized : target.paths || []).filter((p) => /\.go$/.test(p) && !/_test\.go$/.test(p)) : []
const known = fastFiles.map((file) => ({ file, symptom: TASK }))
if (known.length) { decisions.push('fast path: ' + known.length + ' file(s) passed to code-workers as known'); log('fast path: known ' + known.length + ' file(s)') }
const ev = EVAL ? await runChild('code-evaluator', { base: a.base, skill: a.skill, scenarios: a.scenarios, seeds: a.seeds, conditions: a.conditions, steps: stepsFor({ run: 'run', judge: 'judge' }), profile: a.profile, provider: a.provider }) : null
if (EVAL) decisions.push('analytical: code-evaluator ' + (ev && !ev.error ? 'scored ' + ((ev.cells || []).length) + ' cell(s)' : 'failed: ' + ((ev && ev.error) || 'returned nothing')))
const scanned = SCAN ? await scanFirst() : null
if (scanned && scanned.error) return ERR(scanned.error, tier, route, decisions)
// a blocked/incomplete/stalled/capped scan never reads as clean, even with nothing to fix
const SCAN_EXIT = scanned && KEEP.includes(scanned.exit) ? scanned.exit : ''
if (scanned && (!scanned.known.length || a.fix === false)) return await record(withEval(await sharedGate({ exit: SCAN_EXIT || (scanned.known.length ? 'review' : 'green'), tier, route, packages: 0, merged: scanned.confirmed.length, confirmed: scanned.confirmed, dropped: [], unverified: scanned.unverified, fixed: [], red: [], files: [], decisions, report: head + '\n' + scanned.report })))
const jobs = splitJobs(scanned)
if (!jobs.length) return ERR('nothing to run: no package or path to work on (sizing returned none); pass args.paths or args.packages', tier, route, decisions)
if (GATE && a.fix === false) decisions.push('shared gate: fix=false, no green-keeper-loop child; checked once (no edits) at the end')
if (GATE) {
  const clash = GATE_SCOPE.filter((d) => jobs.some((j) => j.group.some((g) => overlaps(d, g))))
  if (clash.length) return ERR('args.gateScope ' + clash.join(', ') + ' overlaps a package group; the layers must be file-disjoint', tier, route, decisions)
  if (a.fix !== false) jobs.push({ child: 'green-keeper-loop', group: GATE_SCOPE, args: { cwd: CWD, gate: GATE, scope: GATE_SCOPE, steps: CHILD_STEPS && CHILD_STEPS.fix ? { fix: CHILD_STEPS.fix } : undefined, profile: a.profile, provider: a.provider } })
}
if (jobs.length > 1) decisions.push('fan-out (parallel): ' + jobs.map((j) => j.child + ' [' + j.group.join(', ') + ']').join(' ‖ '))
const runs = await parallel(jobs.map((j) => () => runChild(j.child, j.args)))
let res = merge(jobs.map((j, i) => ({ ...j, r: runs[i] })))
if (res.exit === 'error') return res
if (a.fix !== false && res.foreign.length) res = await repairForeign(res)
if (a.fix !== false && a.loop !== false) res = await loopIfNeeded(res, jobs.filter((j) => j.child === 'code-workers').length)
const pfiles = [...new Set([...touchedFiles(res.fixed), ...res.files])].sort()
const POLISH = a.polish !== false && a.fix !== false
if (POLISH && pfiles.length && lowBudget()) res.decisions.push('polish skipped: budget under 150k tokens remaining')
else if (POLISH && pfiles.length) {
  // the polish prompts all build on ctx.rules (the stage block is copied verbatim), so the protected task rides there
  const keep = TASK ? ' Protected requirement: the task below is the owner\'s requested change; never revert, inline away, rename back or otherwise undo what it asks for; flag instead.\nTask: ' + TASK + '\n' : ''
  const po = await stagePolish({ ...CTX, rules: CTX.rules + keep }, { files: pfiles })
  const fs = po.review.findings
  const sev = ['critical', 'high', 'medium', 'low'].map((k) => [k, fs.filter((f) => f.severity === k).length]).filter((x) => x[1])
  const tally = 'review: ' + fs.length + ' finding(s) (' + (sev.map((x) => x[1] + ' ' + x[0]).join(', ') || 'none') + ')'
  res = {
    ...res, polish: po, exit: po.red.length ? 'red' : res.exit, red: [...new Set([...res.red, ...po.red])], files: [...new Set([...res.files, ...po.files])],
    decisions: [...res.decisions, ...po.decisions.map((l) => (po.review.ran && /^review: \d/.test(l) ? tally : l))], report: res.report + po.report,
  }
}
if (SCAN_EXIT && res.exit === 'green') res = { ...res, exit: SCAN_EXIT, decisions: [...res.decisions, 'scan exit ' + SCAN_EXIT + ': not green'] }
if (IMPL_EMPTY && res.exit === 'green') res = { ...res, exit: 'incomplete', decisions: [...res.decisions, 'implementer changed no file: the requested change was not made, exit incomplete'] }
if (IMPL_NO_LIVE && res.exit === 'green') res = { ...res, exit: 'review', decisions: [...res.decisions, 'implementer reported no live run: exit review'] }
return await record(withEval(await sharedGate(res)))

// sharedGate: one check-only re-run of the shared gate after every child and the polish (either may change it).
async function sharedGate(r0) {
  if (!GATE) return r0
  const sg = await stageGate(CTX, { gate: GATE, scope: GATE_SCOPE, check: CS.gate, max: 0, label: 'shared-gate' })
  const d = [...r0.decisions, 'shared gate re-check after all children and polish: ' + (sg.exit === 'green' ? 'green' : 'red: ' + sg.residual.slice(0, 1).join('; '))]
  return sg.exit === 'green' ? { ...r0, decisions: d } : { ...r0, exit: 'red', red: [...new Set([...r0.red, ...GATE_SCOPE])], decisions: d }
}

function overlaps(x, y) { return x === y || x.startsWith(y + '/') || y.startsWith(x + '/') }

// splitJobs cuts the target's top-level dirs into ≤3 contiguous groups, one code-workers child each; groups never share a tree.
function splitJobs(sc) {
  // every dir the scan named: code-workers applies known only to the exact packages it is given, never to nested ones
  const exact = sc ? [...new Set(sc.known.map((x) => stDir(stRel(CTX, x.file))))].sort() : null
  const scanDirs = exact ? topDirs(exact) : null
  const tops = (scanDirs || topDirs(target.packages || dirsOf(target.paths))).sort()
  const kn = sc ? sc.known : known
  const n = tops.some((d) => d === '' || d === '.') ? 1 : Math.min(3, tops.length) // a root package's gate spans every dir
  return Array.from({ length: n }, (_, i) => tops.slice(Math.floor((i * tops.length) / n), Math.floor(((i + 1) * tops.length) / n))).map((g) => {
    const mine = (f) => g.some((d) => overlaps(dirsOf([f])[0], d))
    const k = kn.filter((x) => mine(x.file))
    const t = exact ? { packages: exact.filter((x) => g.some((top) => overlaps(x, top))) } : target.packages ? { packages: g } : { paths: target.paths.filter(mine) }
    return { child: 'code-workers', group: g, args: { cwd: CWD, ...t, fix: a.fix, minFix: a.minFix, focus, steps: CHILD_STEPS, profile: a.profile, provider: a.provider, polish: false, gate: PKG_GATE, ...(k.length ? { known: k } : {}) } }
  })
}

// merge folds the children's results into one; a failed child turns its group red, and only an all-failed run is an error.
function merge(done) {
  const bad = (r) => !r || r.exit === 'error'
  const code = done.filter((x) => x.child !== 'green-keeper-loop')
  if (code.every((x) => bad(x.r))) return ERR((code[0].r && code[0].r.error) || code[0].child + ' returned nothing', tier, route, decisions)
  const ok = code.filter((x) => !bad(x.r))
  const cat = (k) => ok.flatMap((x) => x.r[k] || [])
  const tag = (x, d) => (code.length > 1 ? '[' + x.group.join(', ') + '] ' + d : d)
  const d = [...decisions, ...ok.flatMap((x) => (x.r.decisions || []).map((l) => tag(x, l)))]
  const red = cat('red')
  for (const x of code.filter((y) => bad(y.r))) { d.push(tag(x, x.child + ' failed: ' + ((x.r && x.r.error) || 'returned nothing'))); red.push(...x.group) }
  const files = cat('files')
  const gk = done.find((x) => x.child === 'green-keeper-loop')
  if (gk) {
    const g = gk.r || { exit: 'error', iterations: 0, filesChanged: [], residual: ['green-keeper-loop returned nothing'] }
    d.push('shared gate ' + g.exit + (g.iterations ? ' after ' + g.iterations + ' fix iteration(s)' : '') + ' over ' + gk.group.join(', ') + (g.exit === 'green' ? '' : ': ' + (g.residual || []).slice(0, 3).join('; ')))
    if (g.exit !== 'green') red.push(...gk.group)
    files.push(...strs(g.filesChanged))
  }
  // a child that ended blocked/stalled/capped/incomplete (or red with no red list) never merges to green
  const kept = ok.filter((x) => KEEP.includes(x.r.exit)).sort((x, y) => KEEP.indexOf(x.r.exit) - KEEP.indexOf(y.r.exit))
  for (const x of kept) d.push(tag(x, x.child + ' exit ' + x.r.exit + ': not green'))
  for (const x of ok.filter((y) => y.r.exit === 'red' && !strs(y.r.red).length)) red.push(...x.group)
  // a finding cited outside its child's group that the file's owning group already confirmed is one bug: drop the copy
  const owns = (x, f) => x.group.some((dd) => overlaps(dirsOf([stRel(CTX, f.file)])[0], dd))
  const dupOwned = (x, f) => !owns(x, f) && ok.some((o) => o !== x && owns(o, f) && (o.r.confirmed || []).some((g) => sameBug(CTX, g, f)))
  const confirmed = ok.flatMap((x) => (x.r.confirmed || []).filter((f) => !dupOwned(x, f))).sort((x, y) => ORDER[x.severity] - ORDER[y.severity])
  const uniq = [...new Set(red)]
  return {
    exit: uniq.length ? 'red' : kept.length ? kept[0].r.exit : a.fix === false ? 'review' : 'green', tier, route, packages: ok.reduce((n, x) => n + (x.r.packages || 0), 0), merged: ok.reduce((n, x) => n + (x.r.merged || 0), 0),
    confirmed, dropped: cat('dropped'), unverified: cat('unverified'), fixed: cat('fixed'), foreign: ok.flatMap((x) => (x.r.foreign || []).filter((f) => !dupOwned(x, f))), red: uniq, files: [...new Set(files.map((f) => stRel(CTX, f)))],
    decisions: d, report: head + '\n' + ok.map((x) => x.r.report || '').join('\n\n'),
  }
}

// scanFirst: code-analyzer audits the target review-only; its confirmed findings become code-workers' known bugs (Find and Rederive skipped).
async function scanFirst() {
  const where = ANALYZE ? { range: RANGE } : { paths: target.packages || target.paths }
  const sc = await runChild('code-analyzer', { cwd: CWD, ...where, fix: false, minFix: a.minFix || 'low', focus, dimensions: a.dimensions, steps: stepsFor({ find: 'audit', rederive: 'refute' }), profile: a.profile, provider: a.provider })
  if (!sc || sc.exit === 'error') return { error: 'code-analyzer scan failed: ' + ((sc && sc.error) || 'returned nothing') }
  const confirmed = sc.confirmed || []
  const known = confirmed.map((f) => ({ file: stRel(CTX, f.file), severity: f.severity, symptom: (f.title || f.symptom) + (f.scenario ? ' — ' + f.scenario : '') }))
  decisions.push('analytical: code-analyzer scan exit ' + sc.exit + ', ' + confirmed.length + ' confirmed, ' + (sc.refuted || []).length + ' refuted → ' + (known.length ? 'code-workers fix them' : 'nothing to fix'))
  return { exit: sc.exit, known, confirmed, unverified: sc.unverified || [], report: sc.report || '' }
}

// loopIfNeeded: green-keeper-loop over the dirs whose fix landed but gate stayed red, and over every touched dir when 2+ children ran.
async function loopIfNeeded(r0, nChildren) {
  const gateRed = [...new Set((r0.fixed || []).filter((f) => f.impl && f.impl.status === 'done' && f.gate && f.gate.exit !== 'green').map((f) => f.pkg))]
  const touched = [...new Set([...touchedFiles(r0.fixed), ...r0.files].filter((f) => /\.go$/.test(f)).map((f) => stDir(stRel(CTX, f))))]
  const scope = [...new Set([...gateRed, ...(nChildren > 1 ? touched : [])])].filter((d) => !GATE_SCOPE.some((g) => overlaps(d, g))).sort()
  if (!scope.length) return r0
  const gk = await runChild('green-keeper-loop', { cwd: CWD, scope, gate: PKG_GATE, steps: stepsFor({ fix: 'fix' }), profile: a.profile, provider: a.provider })
  const green = !!gk && gk.exit === 'green'
  const red = green ? r0.red.filter((d) => !gateRed.includes(d)) : [...new Set([...r0.red, ...scope])]
  const why = gateRed.length ? 'gate red in ' + gateRed.join(', ') : nChildren + ' children shared the run'
  return { ...r0, exit: red.length ? 'red' : r0.exit === 'red' ? 'green' : r0.exit, red, files: [...new Set([...r0.files, ...strs(gk && gk.filesChanged).map((f) => stRel(CTX, f))])],
    decisions: [...r0.decisions, 'loop (' + why + '): green-keeper-loop over ' + scope.join(', ') + ' exit ' + ((gk && gk.exit) || 'none') + (green ? '' : ': ' + strs(gk && gk.residual).slice(0, 2).join('; '))] }
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

// record appends the run's report and decisions to the repo's memory reports so continuity needs no hand-written notes.
async function record(res0) {
  const files = [...new Set([...touchedFiles(res0.fixed), ...strs(res0.files)])].sort()
  const res = { ...res0, files, report: res0.report + (files.length ? '\n\nFiles touched: ' + files.join(', ') : '') }
  if (a.record === false) return { ...res, coworkers: COWORKERS }
  const label = String(a.label || 'code-engineer').replace(/[^A-Za-z0-9._-]+/g, '-')
  const recordOnce = () => agent(
    S.record.task(RULES + ' Record this run and nothing else. Write the JSON below verbatim to a NEW file from `mktemp "${TMPDIR:-/tmp}/record.XXXXXX"` ' +
      '(Read that empty file before you Write it), then run exactly `cd ' + CWD + ' && claude-code memory report --label ' + label + ' --file <that file> --format json </dev/null` ' +
      'and return its paths array. Edit nothing else.\nJSON:\n' + JSON.stringify({ report: res.report, decisions: res.decisions })),
    { phase: 'Record', label: 'record', schema: S.record.schema(RECORDED), model: S.record.model, effort: S.record.effort, ...S.record.type },
  ).catch(() => null)
  let w = S.record.done('record', await recordOnce())
  if (!w || !(w.paths || []).length) w = S.record.done('record', await recordOnce()) // a no-op record (paths []) gets one retry
  return { ...res, coworkers: COWORKERS, recorded: w ? w.paths : [], decisions: [...res.decisions, w ? 'recorded: ' + w.paths.join(', ') : 'record agent returned nothing'] }
}
