export const meta = {
  name: 'code-engineer',
  description: 'One size-routed entry point: size the job, fan it out to 1-3 parallel code-workers children on disjoint dir groups (plus a green-keeper-loop on a shared gate), code-analyzer for a bare range or code-evaluator for a skill comparison; then one polish over the merged diff (or a plan for an epic)',
  whenToUse:
    'USE: any code job when you do not want to pick a workflow; sizes it (TRIVIAL/LOW/MEDIUM/HIGH/EPIC) and routes. CALL: Workflow({scriptPath: "~/.claude/workflows/code-engineer.js", args: {cwd, task?, packages?, paths?, range?, skill?}}) with at least one of task/packages/paths/range/skill (range = a git range like main..HEAD; a bare range runs code-analyzer, with a task, paths or packages its changed Go files become paths). OPTIONAL: tier (skips sizing), fix (default true), minFix (default low: every finding is fixed, decision #54), focus, label (report file name, default code-engineer), record (default true; false skips the Record step), steps {size, intel, impl, plan, dedupe, simplify, review, record, child} = {agent?, model?, effort?, profile?, provider?} per stage (agent claude|codex|copilot|gemini|opencode; external stages run on the session\'s route, or args/steps profile|provider, and fall back on 429/auth; each reports ran on or BLOCKED). OPTIONAL: profile, provider (run-wide override for external stages). child = the routed child\'s own steps (simplify/review/dedupe are rejected there: code-engineer polishes once); its fix, test and gate also drive the polish Fix-all. Fan-out: the target\'s top-level dirs split into at most 3 file-disjoint groups, one code-workers child each (polish:false), all launched in parallel. gate (a shared gate command) with gateScope (dirs its fixer may edit, disjoint from the groups): one green-keeper-loop child runs beside them. skill (+ base, scenarios, seeds, conditions): a skill/route comparison, routed to code-evaluator. polish (default true; false skips it) runs once after the children over the merged diff: Dedupe (2+ packages), Simplify, Review; each package with review findings gets one Fix-all, guard test and gate (no second child run). fast (default true; false disables): a LOW/MEDIUM fix task passes its files to code-workers as known, skipping Find and Rederive. A low token budget (under 150k left) skips the polish. RETURNS {exit, tier, route, packages, merged, confirmed, dropped, unverified, fixed, red, files (sorted repo-relative files the implementer, fixers, gate fixers and polish changed), polish, decisions, report} (runs, cells on the code-evaluator route). Details: saved-workflows.md.',
  phases: [
    { title: 'Size', detail: 'one cheap agent reads the task, git status/diff and go list and returns the tier, packages and paths' },
    { title: 'Route', detail: 'MEDIUM adds an intel agent, a feature task adds an implementer, EPIC drafts a plan and stops' },
    { title: 'Run', detail: '1-3 code-workers children on disjoint dir groups in parallel (plus green-keeper-loop on args.gate); a bare range runs code-analyzer, a skill comparison code-evaluator' },
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
// ── stages: copied verbatim into code-analyzer, green-keeper-loop, code-workers, code-engineer ──
// Shared stages, called in-script so workflow() nesting stays one level; ctx = {cwd, cd, rules, S, model?, gateMax?}.
const ST_NEAR = 10
const ST_GATE_MAX = 2
const stRel = (ctx, f) => String(f || '').replace(ctx.cwd ? ctx.cwd + '/' : /^$/, '').replace(/^\.\//, '')
const stDir = (f) => String(f).split('/').slice(0, -1).join('/') || '.'
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
    source: { type: 'string', enum: ['Makefile', 'go.mod', 'pyproject', 'package.json', 'Cargo.toml', 'none'] },
  },
}
// stageAsk runs one stage agent; a thrown call becomes null, and st.done records an external stage's ran-on.
const stageAsk = (ctx, st, label, ph, prompt, schema, pin) => agent(st.task(prompt), {
  label, phase: ph, schema: st.schema(schema), ...st.type,
  model: (ctx.model || ((m) => m))((!st.ext && pin && pin.model) || st.model), effort: (pin && pin.effort) || st.effort,
}).catch(() => null).then((r) => st.done(label, r))
// stageDetect picks a scoped verify gate from the toolchain file; gate '' when none.
async function stageDetect(ctx, st, scope) {
  phase('Detect')
  const d = await stageAsk(ctx, st, 'detect', 'Detect', `${ctx.cd}Pick this repo's verify gate; do NOT run it. First call: ls Makefile go.mod pyproject.toml setup.cfg package.json Cargo.toml .venv 2>/dev/null; then read only the file that decides (at most 3 tool calls). ` +
    `The toolchain file decides; a Makefile only ever PREPENDS a lint step. Scope dirs: ${scope.join(' ') || '(repo root)'}. ` +
    `(1) go.mod → "go build ./... && go vet ./<dir>/... && go test ./<dir>/..." over the scope dirs (./... when the scope is the root); ` +
    `(2) pyproject.toml/setup.cfg → "ruff check <dir> && pytest -q <dir-or-tests>" (ruff only if configured; prefix each with .venv/bin/python -m when .venv exists); ` +
    `(3) package.json → "npm run lint && npm test -- <scope paths>" using only scripts that exist; (4) Cargo.toml → "cargo build && cargo test -p <crate of each scope dir>". ` +
    `If a Makefile has a lint (or fmt/vet) target with a report-only form (e.g. its help says CHECK=1 reports), return "make lint CHECK=1 && " + that command and source "Makefile"; ` +
    `skip a lint target that only auto-fixes — it edits files outside the scope, and other sessions share this checkout. Never return make test/audit/check/install/deploy/release/e2e — a suite-wide target is not a scoped gate. ` +
    `(5) Makefile only, no toolchain file, or no scoped command derivable → gate "" and source "none" (the caller must pass args.gate).`, ST_DETECT)
  return { gate: (d && d.gate) || '', source: (d && d.source) || 'none' }
}
// stageGate: an optional cheap check, then up to o.max fixer iterations over o.scope; exit green|stalled|capped|error.
async function stageGate(ctx, o) {
  const tag = o.label || 'gate'
  const cmd = o.gate || o.scope.map((p) => 'go build ./' + p + '/... && go vet ./' + p + '/... && go test -count=1 ./' + p + '/...').join(' && ')
  const out = (status, exit, hist, residual) => {
    if (status !== 'green' && hist.length) log(tag + ' still ' + status + ' after ' + hist.length + ' iteration(s); residual: ' + residual.length)
    return { status, exit, gate: cmd, iterations: hist.length, filesChanged: [...new Set(hist.flatMap((h) => h.filesChanged))], residual, history: hist }
  }
  if (o.check) {
    const c = await stageAsk(ctx, o.check, tag, 'Gate', ctx.rules + ' Run and report: `set -o pipefail; ' + cmd + '`' +
      (o.schemas ? '; if `git diff -- ' + o.scope.join(' ') + '` changes a function that writes a file, also `go test -count=1 ./internal/schemas/`' : '') + '. green=true only if every command exited 0. Edit nothing.', ST_CHECK)
    if (c && c.green) return out('green', 'green', [], [])
    if (!o.max) return out('red', 'capped', [], [c ? c.output : 'gate check returned nothing'])
  }
  const hist = []
  for (let i = 1; i <= (o.max || 0); i++) {
    const last = hist[hist.length - 1]
    const prior = last && last.residual.length ? '\nStill red after iteration ' + (i - 1) + ':\n- ' + last.residual.join('\n- ') + '\n' : ''
    const r = (await stageAsk(ctx, o.fix, tag + ':' + i, 'Gate', 'Iteration ' + i + '/' + o.max + ' of a gate loop. ' + ctx.cd +
      'Method: ~/.claude/skills/problem-solving/references/verify.md (Gate Loop)\nGate: ' + cmd + '\nEdit ONLY under: ' + o.scope.join(', ') + '\nRules: ' + ctx.rules + '\n' + prior +
      '\nProtocol: run the gate and paste its tail; if exit 0 → status="green" and stop. Otherwise fix the FIRST failure per the Method, re-run the gate; repeat for at most ' + (o.fixes || 3) +
      ' distinct failures this iteration, then report (the rest go into residual verbatim). A failure outside scope goes into residual verbatim.' +
      ' If the gate cannot run at all (permission denied, missing command), set status="error", residual to the exact refusal, and stop.', ST_LOOP)) ||
      { status: 'error', failuresBefore: 0, failuresAfter: 0, filesChanged: [], residual: ['agent returned no result'], summary: 'no result' }
    hist.push({ iteration: i, ...r })
    log(tag + ' ' + i + '/' + o.max + ': ' + r.status + ' (' + r.failuresBefore + ' → ' + r.failuresAfter + ') changed=' + r.filesChanged.length)
    if (r.status === 'green') return out('green', 'green', hist, r.residual)
    if (r.status === 'error') return out('error', 'error', hist, r.residual) // an unrunnable gate never improves by retrying
    if (last && last.status === r.status && r.failuresBefore === r.failuresAfter && last.failuresAfter === r.failuresAfter) {
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
    : ' Edit only non-test files under ' + o.pkg + ', except a *_test.go file a cost finding cites.'
  const impl = await stageAsk(ctx, ctx.S.fix, 'fix:' + o.label, 'Fix', ctx.rules + ' Fix these confirmed defects in ' + o.pkg + ' and nothing else.' + own +
    ' The evidence is verified: do not re-audit; first tool call: cat the cited files. The Test stage writes any requested test; that is not unfinished work, so report done. ' +
    'Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 4-6). Root cause first, smallest correct change, functions < 50 lines, 1-line comments. ' +
    'Grep every caller outside the package of each function you changed and report in `callers` whether any now duplicates or contradicts the new behavior (do not edit them). ' +
    'Run the repo formatter and linter on what you touched' + (o.owns ? '' : ' (gofmt + go vet ./' + o.pkg + '/... for Go)') + '. Return `files`: every repo-relative file you changed.\n' + list, ST_WORK, o.pin)
  const done = !!(impl && impl.status === 'done')
  const test = done && o.tests !== false ? await stageAsk(ctx, ctx.S.test, 'test:' + o.label, 'Test', ctx.rules + ' Fixes just landed in ' + o.pkg + ' (`git diff -- ' + o.pkg + '`; changed: ' + (impl.files || []).join(', ') + '). ' +
    'Repair tests the fixes break. Edit only test files; each test under complexity 10. Skip comment-only and dead-code fixes. Put "n/a" in callers.' +
    (bugs.length ? '\nWrite ONE regression test per bug fix that calls the real code and fails without the fix; for Go prove each with `claude-code test mutate <file> <old> <new> --run <RE> ./' + o.pkg + ' </dev/null` (exit 0 = caught).\n' + bugs.map(item).join('\n') : '') +
    (costs.length ? '\nFor each cost fix, pin the unchanged behavior it must keep: a test that calls the real code and passes before and after the fix ' +
      '(it need not fail without it; no mutate). When existing tests already pin that behavior, name them in summary and add none.\n' + costs.map(item).join('\n') : ''), ST_WORK) : null
  const files = [...new Set([...((impl && impl.files) || []), ...((test && test.files) || [])].map((f) => stRel(ctx, f)))]
  return { pkg: o.pkg, impl, test, done, testOk: o.tests === false || !!(test && test.status !== 'blocked'), files }
}
// stagePolish: once over a merged diff: Dedupe (2+ packages), Simplify, Review, then per package with findings one stageFix and stageGate.
async function stagePolish(ctx, o) {
  const files = [...new Set(o.files.map((f) => stRel(ctx, f)))]
  const pkgs = [...new Set(files.map(stDir))]
  const scope = ' Only these files (the tree is shared; never touch or diff others): ' + files.join(' ') + '. Diff: `git diff -- ' + files.join(' ') + '`.'
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
  for (const f of findings) if (/\.go$/.test(f.file) && !/_test\.go$/.test(f.file) && pkgs.includes(stDir(f.file))) (byPkg[stDir(f.file)] = byPkg[stDir(f.file)] || []).push(f)
  const fixall = (await parallel(Object.keys(byPkg).map((p) => async () => {
    const fx = await stageFix(ctx, { pkg: p, findings: byPkg[p], label: 'fixall:' + p })
    const g = fx.done ? await stageGate(ctx, { scope: [p], check: ctx.S.gate, fix: ctx.S.fix, max: ctx.gateMax || 0, schemas: true, label: 'regate:' + p }) : null
    return { pkg: p, findings: byPkg[p].length, done: fx.done, green: fx.done && fx.testOk && !!g && g.exit === 'green', files: [...fx.files, ...(g ? g.filesChanged : [])] }
  }))).filter(Boolean)
  for (const x of fixall) lines.push('fix-all ' + x.pkg + ': ' + (x.done ? x.findings + ' fixed' : 'NOT done') + ', gate ' + (x.green ? 'green' : 'red'))
  const report = ['', '## Polish', ...lines.map((l) => '- ' + l), ...findings.map((f) => '- **' + f.severity + '** `' + f.file + ':' + f.line + '` (' + f.symbol + ') — ' + f.symptom)].join('\n')
  return { dedupe, simplify, review: { findings, ran: !!rv }, fixall, green: fixall.every((x) => x.green), red: fixall.filter((x) => !x.green).map((x) => x.pkg), files: [...new Set(fixall.flatMap((x) => x.files))], decisions: lines, report }
}
// ── end stages ──

const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const CWD = a.cwd || ''
const TASK = a.task ? String(a.task) : ''
const ERR = (m, tier, route, decisions) => ({ exit: 'error', error: 'code-engineer: ' + m, tier: tier || '', route: route || '', decisions: decisions || [], report: '' })
const strs = (v) => (Array.isArray(v) ? v.filter(Boolean).map(String) : [])
let PATHS = strs(a.paths)
let PKGS = strs(a.packages)
const TIERS = ['TRIVIAL', 'LOW', 'MEDIUM', 'HIGH', 'EPIC']
const RANGE = a.range ? String(a.range) : ''
const EVAL = !!a.skill
if (RANGE && !/^[A-Za-z0-9._\/~^@{}-]+(\.\.\.?[A-Za-z0-9._\/~^@{}-]+)?$/.test(RANGE)) return ERR('args.range must be a git range like main..HEAD')
if (!CWD || !(TASK || PATHS.length || PKGS.length || RANGE || EVAL)) return ERR('args.cwd and one of args.task, args.packages, args.paths, args.range or args.skill are required')
if (a.tier !== undefined && !TIERS.includes(a.tier)) return ERR('args.tier must be ' + TIERS.join('|'))
// A bare range (no task, paths or packages) is a diff audit, so code-analyzer owns it (D2).
const ANALYZE = !!RANGE && !TASK && !PATHS.length && !PKGS.length && !EVAL
// args.gate is a shared gate: one green-keeper-loop child keeps it green beside the code-workers children, editing only gateScope.
const GATE = a.gate ? String(a.gate) : ''
const GATE_SCOPE = strs(a.gateScope).map((d) => d.replace(/\/+$/, ''))
if (GATE && !GATE_SCOPE.length) return ERR('args.gateScope is required with args.gate: the dirs its fixer may edit, disjoint from the package groups')
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

const RULES =
  'Shared live tree: never git stash|checkout|reset|restore|clean|commit|add, never make build|install|audit, never edit settings.json. ' +
  'cwd: ' + CWD + ' (external agents: ask --workdir ' + CWD + '). Run each command bare, never as `cd … && cmd`: copilot\'s shell rejects compound commands.'
const CTX = { cwd: CWD, cd: '', rules: RULES, S: { ...CS, dedupe: S.dedupe, simplify: S.simplify, review: S.review }, gateMax: ST_GATE_MAX }
const SIZED = {
  type: 'object',
  properties: {
    kind: { type: 'string', enum: ['fix', 'feature', 'audit', 'architecture'] },
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
  if (dirs.size > 1 || files.length > 5) return 'HIGH'
  return files.length >= 2 ? 'MEDIUM' : 'LOW'
}
const INTEL = { type: 'object', properties: { paths: { type: 'array', items: { type: 'string' } }, focus: { type: 'string' } }, required: ['paths', 'focus'] }
const PLAN = { type: 'object', properties: { plan: { type: 'string' } }, required: ['plan'] }
const RECORDED = { type: 'object', properties: { paths: { type: 'array', items: { type: 'string' } } }, required: ['paths'] }
const ORDER = { critical: 0, high: 1, medium: 2, low: 3 }

// ── Size ──
phase('Size')
const decisions = []
let IMPL_FILES = [] // declared before any record() call: touchedFiles reads it on the early returns too
let tier = a.tier
let focus = a.focus || TASK
let KIND = ''
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
    tier = dirs.size > 1 || PATHS.length > 5 ? 'HIGH' : 'LOW'
    if (tier === 'HIGH' && !PKGS.length) PKGS = [...dirs]
    decisions.push('sized as ' + tier + ': range spans ' + dirs.size + ' package(s), ' + PATHS.length + ' file(s)')
  }
}
if (ANALYZE || EVAL) {
  tier = a.tier || (ANALYZE ? 'HIGH' : 'MEDIUM')
  decisions.push('sized as ' + tier + ': ' + (ANALYZE ? 'a bare range is a diff audit (code-analyzer)' : 'a skill/route comparison (code-evaluator)'))
} else if (!tier && !TASK) {
  tier = PKGS.length ? 'HIGH' : 'LOW'
  decisions.push('sized as ' + tier + ': no task, ' + (PKGS.length ? 'a package list is an audit' : 'only named paths'))
} else if (!tier) {
  const sz = S.size.done('size', await agent(
    S.size.task(RULES + ' Read-only: edit nothing. Do NOT pick a size; find facts. Read the code the task points at and return: kind (fix = repair ' +
      'existing behavior, feature = add behavior, audit = review packages for bugs, architecture = a multi-session design change); the ' +
      'non-test source FILES that must change (repo-relative, one entry per file, never a directory, tests excluded); the Go package dirs ' +
      'only for an audit; a one-line focus; and a one-line reason. Use `git status --short` and `go list ./...` only as needed.\nTask: ' + TASK +
      (PKGS.length ? '\nGiven packages: ' + PKGS.join(', ') : '') + (PATHS.length ? '\nGiven paths: ' + PATHS.join(', ') : '')),
    { phase: 'Size', label: 'size', schema: S.size.schema(SIZED), model: S.size.model, effort: S.size.effort, ...S.size.type },
  ).catch(() => null))
  if (!sz) return ERR('sizing agent returned nothing; pass args.tier to skip it')
  KIND = sz.kind
  if (!PKGS.length) PKGS = strs(sz.packages)
  if (!PATHS.length) PATHS = strs(sz.paths).filter((p) => !/_test\.go$/.test(p))
  tier = tierOf(sz.kind, PATHS, sz.kind === 'audit' ? PKGS : [])
  log('size: kind ' + sz.kind + ', ' + PATHS.length + ' file(s), ' + PKGS.length + ' package(s) -> ' + tier)
  decisions.push('sized as ' + tier + ' (rule: ' + sz.kind + ', ' + PATHS.length + ' file(s)): ' + sz.reason)
  if (!a.focus && sz.focus) focus = sz.focus
} else if (a.tier) decisions.push('sized as ' + tier + ': args.tier given')

// ── Route ──
const route = EVAL ? 'code-evaluator' : tier === 'EPIC' ? 'plan' : ANALYZE ? 'code-analyzer:range' : tier === 'HIGH' ? 'code-workers:packages' : 'code-workers:paths'
decisions.push('route: ' + route)
log('route: ' + route + ' (tier ' + tier + ', kind ' + (KIND || 'n/a') + ')')
const head = '# code-engineer: ' + tier + ' → ' + route
phase('Route')
// runChild runs one child workflow one nesting level down; a throw becomes an error result.
const runChild = (child, cargs) => workflow({ scriptPath: CWD + '/workflows/' + child + '.js' }, cargs).catch((e) => ({ exit: 'error', error: child + ' threw: ' + e.message }))
if (EVAL) {
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
  if (intel && strs(intel.paths).length) {
    const own = new Set(dirsOf(sized))
    const got = KIND === 'fix' && sized.length ? strs(intel.paths).filter((p) => own.has(dirsOf([p])[0])) : strs(intel.paths)
    const dropped = strs(intel.paths).length - got.length
    if (dropped) decisions.push('intel: kept ' + got.length + ' path(s) in the named package(s), dropped ' + dropped + ' outside them (context only)')
    target.paths = [...new Set([...sized, ...got])]
    focus = TASK + '. Intel: ' + intel.focus
  } else decisions.push('intel returned nothing; kept the sized paths')
}
// code-workers only fixes bugs: a feature task gets an implementer first, then code-workers audits and hardens the new code.
const feature = KIND ? KIND === 'feature' : /^\W*(build|add|implement|create|introduce|support|write|finish)\b/i.test(TASK) // leading verb only: constraint text mentioning review must not veto
if ((tier === 'LOW' || tier === 'MEDIUM') && TASK && target.paths.length && feature) {
  const impl = S.impl.done('impl', await agent(
    S.impl.task(RULES + ' Make this change in these files (new files in the same package ok) and nothing else; smallest correct change, functions < 50 lines, 1-line comments. ' +
      'Run gofmt and go vet on the package. Do not write tests; code-workers follows with find, fix, guard test and gate. Return `files`: every repo-relative file you changed.\nFiles: ' + target.paths.join(', ') + '\nTask: ' + TASK + '\nContext: ' + focus),
    { phase: 'Route', label: 'impl', schema: S.impl.schema(ST_WORK), model: S.impl.model, effort: S.impl.effort, ...S.impl.type },
  ).catch(() => null))
  if (impl) IMPL_FILES = strs(impl.files).map((f) => stRel(CTX, f))
  decisions.push('implementer ran: ' + (impl ? impl.status + ' — ' + impl.summary : 'returned nothing'))
  decisions.push('feature task: code-workers below is an audit-and-harden pass over the new code, not a feature builder')
}

// ── Run ──
phase('Run')
// Fast path: a LOW/MEDIUM fix task names the bug, so code-workers skips Find and Rederive for those files (audits and features never).
const fastFiles = (tier === 'LOW' || tier === 'MEDIUM') && KIND === 'fix' && TASK && a.fix !== false && a.fast !== false ? (sized.length ? sized : target.paths || []).filter((p) => /\.go$/.test(p) && !/_test\.go$/.test(p)) : []
const known = fastFiles.map((file) => ({ file, symptom: TASK }))
if (known.length) { decisions.push('fast path: ' + known.length + ' file(s) passed to code-workers as known'); log('fast path: known ' + known.length + ' file(s)') }
const jobs = ANALYZE ? [{ child: 'code-analyzer', group: [RANGE], args: { cwd: CWD, range: RANGE, fix: a.fix, minFix: a.minFix || 'low', steps: CHILD_STEPS, profile: a.profile, provider: a.provider } }] : splitJobs()
if (GATE && ANALYZE) decisions.push('shared gate ignored: code-analyzer gates each round in-script')
if (GATE && !ANALYZE) {
  const clash = GATE_SCOPE.filter((d) => jobs.some((j) => j.group.some((g) => overlaps(d, g))))
  if (clash.length) return ERR('args.gateScope ' + clash.join(', ') + ' overlaps a package group; the layers must be file-disjoint', tier, route, decisions)
  jobs.push({ child: 'green-keeper-loop', group: GATE_SCOPE, args: { cwd: CWD, gate: GATE, scope: GATE_SCOPE, steps: CHILD_STEPS && CHILD_STEPS.fix ? { fix: CHILD_STEPS.fix } : undefined, profile: a.profile, provider: a.provider } })
}
if (jobs.length > 1) decisions.push('fan-out (parallel): ' + jobs.map((j) => j.child + ' [' + j.group.join(', ') + ']').join(' ‖ '))
const runs = await parallel(jobs.map((j) => () => runChild(j.child, j.args)))
let res = merge(jobs.map((j, i) => ({ ...j, r: runs[i] })))
if (res.exit === 'error') return res
const pfiles = [...new Set([...touchedFiles(res.fixed), ...res.files])].sort()
const POLISH = a.polish !== false && a.fix !== false
if (POLISH && pfiles.length && lowBudget()) res.decisions.push('polish skipped: budget under 150k tokens remaining')
else if (POLISH && pfiles.length) {
  const po = await stagePolish(CTX, { files: pfiles })
  const fs = po.review.findings
  const sev = ['critical', 'high', 'medium', 'low'].map((k) => [k, fs.filter((f) => f.severity === k).length]).filter((x) => x[1])
  const tally = 'review: ' + fs.length + ' finding(s) (' + (sev.map((x) => x[1] + ' ' + x[0]).join(', ') || 'none') + ')'
  res = {
    ...res, polish: po, exit: po.red.length ? 'red' : res.exit, red: [...new Set([...res.red, ...po.red])], files: [...new Set([...res.files, ...po.files])],
    decisions: [...res.decisions, ...po.decisions.map((l) => (po.review.ran && /^review: \d/.test(l) ? tally : l))], report: res.report + po.report,
  }
}
return await record(res)

function overlaps(x, y) { return x === y || x.startsWith(y + '/') || y.startsWith(x + '/') }

// splitJobs cuts the target's top-level dirs into ≤3 contiguous groups, one code-workers child each; groups never share a tree.
function splitJobs() {
  const tops = topDirs(target.packages || dirsOf(target.paths)).sort()
  const n = tops.some((d) => d === '' || d === '.') ? 1 : Math.min(3, tops.length) // a root package's gate spans every dir
  return Array.from({ length: n }, (_, i) => tops.slice(Math.floor((i * tops.length) / n), Math.floor(((i + 1) * tops.length) / n))).map((g) => {
    const mine = (f) => g.some((d) => overlaps(dirsOf([f])[0], d))
    const k = known.filter((x) => mine(x.file))
    const t = target.packages ? { packages: g } : { paths: target.paths.filter(mine) }
    return { child: 'code-workers', group: g, args: { cwd: CWD, ...t, fix: a.fix, minFix: a.minFix, focus, steps: CHILD_STEPS, profile: a.profile, provider: a.provider, polish: false, ...(k.length ? { known: k } : {}) } }
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
  if (ANALYZE) d.push('code-analyzer exit: ' + ok[0].r.exit)
  const files = cat('files')
  const gk = done.find((x) => x.child === 'green-keeper-loop')
  if (gk) {
    const g = gk.r || { exit: 'error', iterations: 0, filesChanged: [], residual: ['green-keeper-loop returned nothing'] }
    d.push('shared gate ' + g.exit + (g.iterations ? ' after ' + g.iterations + ' fix iteration(s)' : '') + ' over ' + gk.group.join(', ') + (g.exit === 'green' ? '' : ': ' + (g.residual || []).slice(0, 3).join('; ')))
    if (g.exit !== 'green') red.push(...gk.group)
    files.push(...strs(g.filesChanged))
  }
  const confirmed = cat('confirmed').sort((x, y) => ORDER[x.severity] - ORDER[y.severity])
  const uniq = [...new Set(red)]
  return {
    exit: uniq.length ? 'red' : a.fix === false ? 'review' : 'green', tier, route, packages: ok.reduce((n, x) => n + (x.r.packages || 0), 0), merged: ok.reduce((n, x) => n + (x.r.merged || 0), 0),
    confirmed, dropped: cat('dropped'), unverified: cat('unverified'), fixed: cat('fixed'), red: uniq, files: [...new Set(files.map((f) => stRel(CTX, f)))],
    decisions: d, report: head + '\n' + ok.map((x) => x.r.report || '').join('\n\n'),
  }
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
    S.record.task(RULES + ' Write two files and nothing else. Run `claude-code memory path` for the store dir D and `date +%F` for DATE. ' +
      '(1) Append (create if missing) the markdown REPORT below, followed by one blank line, to D/reports/DATE-' + label + '.md. ' +
      '(2) Append each DECISION below as a line `- DATE · ' + label + ' · <decision>` to D/reports/DATE-decisions.md (create with a `# Decisions` title). ' +
      'Then run `claude-code memory fmt` so both files pass the docs lint. Never edit any other file. Return the two paths.\nREPORT:\n' + res.report + '\nDECISIONS:\n' + res.decisions.map((d) => '- ' + d).join('\n')),
    { phase: 'Record', label: 'record', schema: S.record.schema(RECORDED), model: S.record.model, effort: S.record.effort, ...S.record.type },
  ).catch(() => null)
  let w = S.record.done('record', await recordOnce())
  if (!w || !(w.paths || []).length) w = S.record.done('record', await recordOnce()) // a no-op record (paths []) gets one retry
  return { ...res, coworkers: COWORKERS, recorded: w ? w.paths : [], decisions: [...res.decisions, w ? 'recorded: ' + w.paths.join(', ') : 'record agent returned nothing'] }
}
