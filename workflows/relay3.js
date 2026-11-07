export const meta = {
  name: 'relay3',
  description: 'Streaming relay: each package flows find → rederive → fix → guard test → gate → simplify → review → fix-all on its own, no barrier between packages',
  whenToUse:
    'USE: audit and repair several Go packages (or a LOW/MEDIUM job\'s files) without a barrier between them; review-only with fix:false. CALL: Workflow({scriptPath: "~/.claude/workflows/relay3.js", args: {cwd, packages: [...] | paths: [...]}}). OPTIONAL: slice (run only the first N packages), fix (default true), minFix (default medium), focus, known [{file, symptom}] (bug already named: skips Find and Rederive for that file\'s package), fast (false ignores known), polish (default true; false skips Simplify, Review and Fix-all; also skipped under 150k budget tokens left), steps {find, rederive, fix, test, gate, simplify, review} = {model, effort} per stage (claude only; fix-all uses steps.fix). RETURNS {exit, route, packages, merged, confirmed, dropped, unverified, fixed (each with simplify, review, fixall), red, decisions, report}. Details: saved-workflows.md.',
  phases: [
    { title: 'Find', detail: 'two lenses per package; claims merged by file ±10 lines or same symbol within the package' },
    { title: 'Rederive', detail: 'each claim re-derived blind from file:line + symptom' },
    { title: 'Fix', detail: 'as soon as a package has confirmed findings at or above minFix, one implementer fixes them' },
    { title: 'Test', detail: 'one guard test per fix, proven with claude-code test mutate' },
    { title: 'Gate', detail: 'package-scoped build/vet/test, plus internal/schemas when a writer changed' },
    { title: 'Simplify', detail: 'after a green gate: review simplify over the package\'s changed files, simplify gate applied (polish:false skips)' },
    { title: 'Review', detail: 'read-only correctness+contract review of the package diff; any finding earns one Fix-all, Test and re-Gate' },
  ],
}

// ── stepOf: copied verbatim into code-analyzer, green-keeper-loop, code-evaluator, relay3, orchestrate ──
// stepOf(steps, name, defaults, {allowed}) -> {model, effort}; claude only, any bad input throws Error naming stage/field.
const stepOf = (steps, name, defaults, opt) => {
  const S_CLAUDE = ['haiku', 'sonnet', 'opus', 'fable']
  const S_EFFORT = ['low', 'medium', 'high', 'xhigh', 'max']
  const S_FIELDS = ['model', 'effort']
  const allowed = (opt && opt.allowed) || []
  const d = defaults || {}
  if (steps !== undefined && (!steps || typeof steps !== 'object' || Array.isArray(steps))) throw new Error('steps must be an object')
  for (const k of Object.keys(steps || {})) if (!allowed.includes(k)) throw new Error(`steps.${k}: unknown stage (allowed: ${allowed.join('|')})`)
  const where = `steps.${name}`
  const v = (steps || {})[name] === undefined ? {} : steps[name]
  if (!v || typeof v !== 'object' || Array.isArray(v)) throw new Error(`${where} must be an object`)
  for (const k of ['agent', 'profile']) if (k in v) throw new Error(`${where}.${k}: external agents were removed from workflows (claude only; use /code-worker:codex etc. outside the workflow)`)
  for (const k of Object.keys(v)) if (!S_FIELDS.includes(k)) throw new Error(`${where}.${k}: unknown field (allowed: ${S_FIELDS.join('|')})`)
  if (v.effort !== undefined && !S_EFFORT.includes(v.effort)) throw new Error(`${where}.effort must be ${S_EFFORT.join('|')}`)
  const model = v.model !== undefined ? v.model : d.model || 'sonnet'
  if (!S_CLAUDE.includes(model)) throw new Error(`${where}.model must be ${S_CLAUDE.join('|')}`)
  return { model, effort: v.effort !== undefined ? v.effort : d.effort || 'medium' }
}
// ── end stepOf ──

const ERR = (m) => ({ exit: 'error', error: 'relay3: ' + m, route: 'relay3', decisions: [], report: '' })
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const CWD = a.cwd || ''
const RAWP = Array.isArray(a.paths) ? a.paths.filter(Boolean).map((p) => String(p).replace(CWD + '/', '').replace(/\/+$/, '')) : []
// A .go path groups under its directory; any other path is taken as a directory.
const dirOf = (p) => (/\.go$/.test(p) ? p.split('/').slice(0, -1).join('/') || '.' : p)
const FILES = {}
for (const p of RAWP) if (/\.go$/.test(p)) (FILES[dirOf(p)] = FILES[dirOf(p)] || []).push(p)
let PKGS = Array.isArray(a.packages) && a.packages.length ? a.packages.filter(Boolean) : [...new Set(RAWP.map(dirOf))]
if (!CWD || !PKGS.length) return ERR('args.cwd and one of args.packages or args.paths are required')
// ── Steps: who runs each stage ──
const STAGES = ['find', 'rederive', 'fix', 'test', 'gate', 'simplify', 'review']
const OPT = { allowed: STAGES }
let S
try {
  S = {
    find: stepOf(a.steps, 'find', { model: 'sonnet', effort: 'medium' }, OPT),
    rederive: stepOf(a.steps, 'rederive', { model: 'sonnet', effort: 'medium' }, OPT),
    fix: stepOf(a.steps, 'fix', { model: 'sonnet', effort: 'medium' }, OPT),
    test: stepOf(a.steps, 'test', { model: 'sonnet', effort: 'medium' }, OPT),
    gate: stepOf(a.steps, 'gate', { model: 'haiku', effort: 'low' }, OPT),
    simplify: stepOf(a.steps, 'simplify', { model: 'sonnet', effort: 'medium' }, OPT),
    review: stepOf(a.steps, 'review', { model: 'sonnet', effort: 'high' }, OPT),
  }
} catch (e) { return ERR(e.message) }
const FIX = a.fix !== false
const POLISH = a.polish !== false
const ORDER = { critical: 0, high: 1, medium: 2, low: 3 }
const MIN_FIX = ORDER[a.minFix] !== undefined ? ORDER[a.minFix] : ORDER.medium
const NEAR = 10
const MIN_SEV = ORDER[a.minFix] !== undefined ? a.minFix : 'medium'
// known: bugs the caller already named; their package skips Find and Rederive (a.fast === false or fix:false disables).
const KNOWN = {}
if (FIX && a.fast !== false && Array.isArray(a.known)) {
  for (const k of a.known) if (k && k.file && k.symptom) (KNOWN[dirOf(String(k.file).replace(CWD + '/', ''))] = KNOWN[dirOf(String(k.file).replace(CWD + '/', ''))] || []).push(k)
}
const FOCUS = a.focus ? ' Focus for this group: ' + a.focus + '.' : ''
const RULES =
  'Shared live tree, other packages are being edited in parallel: never git stash|checkout|reset|restore|clean|commit|add, never ' +
  'make build|install|audit, never edit settings.json. Prefix every shell command with `cd ' + CWD + ' &&`.'
const READONLY = RULES + ' Read-only: edit nothing, never run go test. Skip *_test.go.'

const SEV = { type: 'string', enum: ['critical', 'high', 'medium', 'low'] }
const FINDING = {
  type: 'object',
  properties: { file: { type: 'string' }, line: { type: 'integer' }, symbol: { type: 'string' }, symptom: { type: 'string' }, scenario: { type: 'string' }, severity: SEV },
  required: ['file', 'line', 'symbol', 'symptom', 'scenario', 'severity'],
}
const FINDINGS = { type: 'object', properties: { findings: { type: 'array', items: FINDING } }, required: ['findings'] }
const REDERIVE = { type: 'object', properties: { reproduced: { type: 'boolean' }, scenario: { type: 'string' }, severity: SEV }, required: ['reproduced', 'scenario', 'severity'] }
const WORK = {
  type: 'object',
  properties: { status: { type: 'string', enum: ['done', 'partial', 'blocked'] }, files: { type: 'array', items: { type: 'string' } }, callers: { type: 'string' }, summary: { type: 'string' } },
  required: ['status', 'files', 'callers', 'summary'],
}
const GATE = { type: 'object', properties: { green: { type: 'boolean' }, output: { type: 'string' } }, required: ['green', 'output'] }
const SIMP = {
  type: 'object',
  properties: { applied: { type: 'array', items: { type: 'string' } }, flagged: { type: 'array', items: { type: 'string' } }, deltaLines: { type: 'integer' }, status: { type: 'string', enum: ['done', 'clean', 'partial', 'blocked'] } },
  required: ['applied', 'flagged', 'deltaLines', 'status'],
}
const REVF = {
  type: 'object',
  properties: { findings: { type: 'array', items: { type: 'object', properties: { file: { type: 'string' }, line: { type: 'integer' }, symbol: { type: 'string' }, symptom: { type: 'string' }, severity: SEV }, required: ['file', 'line', 'symbol', 'symptom', 'severity'] } } },
  required: ['findings'],
}
const LENSES = [
  { key: 'correctness', ask: 'wrong results, bad parsing, state that disagrees with itself, broken invariants' },
  { key: 'failure', ask: 'swallowed or mishandled errors, partial writes, records or locks left behind, races, leaked secrets' },
]
const norm = (f) => String(f).replace(CWD + '/', '')
const list = (cs) => cs.map((c) => '- ' + c.file + ':' + c.line + ' (' + c.symbol + ', ' + c.severity + '): ' + c.symptom + '\n  scenario: ' + c.rederived).join('\n')

function merge(raw) {
  const out = []
  for (const f of raw) {
    const dup = out.find((g) => norm(g.file) === norm(f.file) && (Math.abs(g.line - f.line) <= NEAR || g.symbol === f.symbol))
    if (!dup) out.push({ ...f, file: norm(f.file) })
    else if (ORDER[f.severity] < ORDER[dup.severity]) dup.severity = f.severity
  }
  return out
}

async function review(p) {
  if (KNOWN[p]) {
    const confirmed = KNOWN[p].map((k) => ({ file: norm(k.file), line: 0, symbol: 'named by the task', symptom: String(k.symptom), scenario: String(k.symptom), severity: MIN_SEV, rederived: 'named by the task' }))
    log(p + ': fast path, ' + confirmed.length + ' named, find+rederive skipped')
    return { pkg: p, merged: confirmed.length, confirmed, dropped: [], unverified: [], fast: true }
  }
  const finds = await parallel(LENSES.map((l) => () =>
    agent(
      READONLY + ' Package ' + p + ' (its own directory and nested packages).' + (FILES[p] ? ' Focus on these files: ' + FILES[p].join(', ') + '.' : '') + ' Find real bugs of this kind: ' + l.ask + '.' + FOCUS +
        ' Each finding: file, line, function or symbol, a one-line symptom, a concrete failing scenario. Return {findings: []} when clean.',
      { phase: 'Find', label: 'find:' + l.key + ':' + p, schema: FINDINGS, model: S.find.model, effort: S.find.effort },
    )))
  const merged = merge(finds.filter(Boolean).flatMap((r) => r.findings || []))
  const judged = await parallel(merged.map((f, i) => () =>
    agent(
      READONLY + ' A teammate says there is a bug at ' + f.file + ':' + f.line + ' (' + f.symbol + ') with this symptom: ' + f.symptom +
        '. You have not seen their reasoning. Derive your own failing scenario from the code and rate its severity; reproduced=false if you cannot.',
      { phase: 'Rederive', label: 'rederive:' + p + ':' + i, schema: REDERIVE, model: S.rederive.model, effort: S.rederive.effort },
    ).then((r) => ({ f, r }))))
  // A null rederive (rate or API error) is unverified, never refuted.
  const ok = merged.map((f, i) => judged[i] || { f, r: null })
  const unverified = ok.filter((x) => !x.r).map((x) => ({ file: x.f.file, line: x.f.line, symptom: x.f.symptom }))
  const confirmed = ok.filter((x) => x.r && x.r.reproduced).map((x) => ({ ...x.f, severity: x.r.severity, rederived: x.r.scenario }))
  const dropped = ok.filter((x) => x.r && !x.r.reproduced).map((x) => ({ file: x.f.file, line: x.f.line, symptom: x.f.symptom }))
  log(p + ': merged ' + merged.length + ', confirmed ' + confirmed.length + ', dropped ' + dropped.length + ', unverified ' + unverified.length)
  return { pkg: p, merged: merged.length, confirmed, dropped, unverified }
}

// inPkg: a finder may cite a file outside its package; that finding is fixed by the file's own package, never by p.
const inPkg = (file, p) => norm(file).startsWith(norm(p) + '/')

async function repair(rev) {
  const p = rev.pkg
  const cs = rev.confirmed.filter((c) => ORDER[c.severity] <= MIN_FIX && inPkg(c.file, p))
  if (!FIX) return { ...rev, fixed: 0, decisions: [] }
  if (!cs.length) return { ...rev, fixed: 0, decisions: ['no finding at or above minFix in ' + p] }
  log(p + ': ' + cs.length + ' finding(s) at or above minFix, fixing now')
  const impl = await agent(
    RULES + ' Fix these confirmed bugs in package ' + p + ' and nothing else; edit only non-test files under it. Root cause first, smallest ' +
      'correct change, functions < 50 lines, 1-line comments. Before finishing, grep every caller OUTSIDE the package of each function you ' +
      'changed and report in `callers` whether any now duplicates or contradicts the new behavior (do not edit them). Run gofmt and go vet ./' +
      norm(p) + '/....\n' + list(cs),
    { phase: 'Fix', label: 'fix:' + p, schema: WORK, model: S.fix.model, effort: S.fix.effort },
  )
  const test = await agent(
    RULES + ' Fixes just landed in ' + p + ' (`git diff -- ' + p + '`). Write ONE regression test per fix in the package _test.go files that ' +
      'calls the real code and fails without the fix; prove each with `claude-code test mutate <file> <old> <new> --run <RE> ./' + norm(p) +
      ' </dev/null` (exit 0 = caught). Edit only *_test.go files under ' + p + '; each test under complexity 10. Put "n/a" in callers.\n' + list(cs),
    { phase: 'Test', label: 'test:' + p, schema: WORK, model: S.test.model, effort: S.test.effort },
  )
  const gate = await agent(
    RULES + ' Run and report: `set -o pipefail; go build ./' + norm(p) + '/... && go vet ./' + norm(p) + '/... && go test -count=1 ./' + norm(p) +
      '/...`; if `git diff -- ' + p + '` changes a function that writes a file, also `go test -count=1 ./internal/schemas/`. green=true only ' +
      'if every command exited 0. Edit nothing.',
    { phase: 'Gate', label: 'gate:' + p, schema: GATE, model: S.gate.model, effort: S.gate.effort },
  )
  // A green gate over unchanged code proves nothing: a fixer that did not finish counts as red.
  const done = !!(impl && impl.status === 'done')
  let green = done && !!(gate && gate.green)
  log(p + ': fixed ' + (done ? cs.length : 0) + '/' + cs.length + ', gate ' + (green ? 'green' : 'red'))
  const decisions = [...(rev.fast ? ['fast path: skipped find+rederive, task names the bug'] : []), (done ? 'fixed ' : 'fix NOT done (' + (impl ? impl.status : 'no result') + ') for ') + cs.length + ' finding(s) in ' + p,
    green ? 'gate green in ' + p : 'gate red in ' + p]
  let extra = {}
  if (done && green && POLISH) {
    const files = [...new Set([...(impl.files || []), ...(test && test.files ? test.files : [])].map(norm))]
    const po = await polish(p, files.length ? files : [norm(p)])
    extra = { simplify: po.simplify, review: po.review, fixall: po.fixall }
    green = po.green
    decisions.push(...po.decisions)
  } else if (done && green) decisions.push('polish skipped: polish=false')
  return { ...rev, fixed: cs.length, impl, test, gate, green, decisions, ...extra }
}

const lowBudget = () => typeof budget !== 'undefined' && !!budget && !!budget.total && budget.remaining() < 150000

// polish: Simplify, then Review; any finding (all severities) gets one Fix-all, Test and re-Gate. Red on a red re-gate or an unfinished fix-all.
async function polish(p, files) {
  if (lowBudget()) {
    log(p + ': polish skipped, low budget')
    return { green: true, decisions: ['polish skipped in ' + p + ': budget under 150k tokens remaining'] }
  }
  const scope = 'Only these files (the tree is shared; never touch or diff others): ' + files.join(' ') + '. Diff: `git diff -- ' + files.join(' ') + '`.'
  const sp = await agent(
    RULES + ' Simplify gate (' + CWD + '/skills/simplify/SKILL.md, quality.md -> Simplify) over package ' + p + '. ' + scope +
      ' Run `claude-code review simplify ' + files.join(' ') + '`, then one in-context pass (reuse, simplification, efficiency, altitude). Name the assumption before every cut; ' +
      'flag instead of cutting when it has no covering test; never cut semantic comments, error context or distinct tests; never fix bugs (flag them). Edit only the listed non-test files under ' + p + '. ' +
      'After edits run gofmt, go vet and go test for ' + p + '; if a test regresses, revert your own edit by hand (never git checkout/restore/stash) and flag it. ' +
      'Return applied[] and flagged[] as `file:line · what · assumption`, deltaLines (net changed lines, negative = removed) and status (clean = nothing to do).',
    { phase: 'Simplify', label: 'simplify:' + p, schema: SIMP, model: S.simplify.model, effort: S.simplify.effort },
  ).catch(() => null)
  const simplify = sp || { applied: [], flagged: [], deltaLines: 0, status: 'blocked' }
  const d = ['simplify: ' + (sp ? simplify.applied.length + ' applied, ' + simplify.flagged.length + ' flagged, Δ ' + simplify.deltaLines + ' lines (' + simplify.status + ')' : 'agent returned nothing') + ' in ' + p]
  const rv = await agent(
    READONLY + ' Review the final diff of package ' + p + ' with the ' + CWD + '/skills/evaluate-code/SKILL.md correctness and contract lenses (wrong results, broken invariants, ' +
      'changed behavior callers rely on, errors dropped). ' + scope + ' Each finding: file, line, symbol, one-line symptom, severity. Return {findings: []} when clean.',
    { phase: 'Review', label: 'review:' + p, schema: REVF, model: S.review.model, effort: S.review.effort },
  ).catch(() => null)
  const findings = rv ? rv.findings : []
  const sev = ['critical', 'high', 'medium', 'low'].map((k) => [k, findings.filter((f) => f.severity === k).length]).filter((x) => x[1])
  const review = { findings, ran: !!rv }
  d.push('review: ' + (rv ? findings.length + ' findings (' + (sev.map((x) => x[1] + ' ' + x[0]).join(', ') || 'none') + ')' : 'agent returned nothing') + ' in ' + p)
  if (!findings.length) return { green: true, simplify, review, fixall: { fixed: 0, status: 'skipped' }, decisions: d }
  const text = findings.map((f) => '- ' + f.file + ':' + f.line + ' (' + f.symbol + ', ' + f.severity + '): ' + f.symptom).join('\n')
  const fx = await agent(
    RULES + ' Fix EVERY review finding below in package ' + p + ', lows included, in one pass; edit only non-test files under it. Root cause first, smallest correct change, ' +
      'functions < 50 lines, 1-line comments. Run gofmt and go vet ./' + norm(p) + '/.... Put "n/a" in callers.\n' + text,
    { phase: 'Fix', label: 'fixall:' + p, schema: WORK, model: S.fix.model, effort: S.fix.effort },
  ).catch(() => null)
  const fixedAll = !!(fx && fx.status === 'done')
  d.push('fix-all: ' + (fixedAll ? findings.length : 0) + ' fixed of ' + findings.length + ' in ' + p + (fixedAll ? '' : ' (NOT done: ' + (fx ? fx.status : 'no result') + ')'))
  if (!fixedAll) return { green: false, simplify, review, fixall: { fixed: 0, status: fx ? fx.status : 'none' }, decisions: d }
  await agent(
    RULES + ' Fix-all just landed in ' + p + ' (`git diff -- ' + p + '`). Write a guard test for each behavior change in the package _test.go files that calls the real code ' +
      'and fails without the change; prove each with `claude-code test mutate <file> <old> <new> --run <RE> ./' + norm(p) + ' </dev/null` (exit 0 = caught). Skip pure style edits. ' +
      'Edit only *_test.go files under ' + p + '; each test under complexity 10. Put "n/a" in callers.\n' + text,
    { phase: 'Test', label: 'fixall-test:' + p, schema: WORK, model: S.test.model, effort: S.test.effort },
  ).catch(() => null)
  const g2 = await agent(
    RULES + ' Run and report: `set -o pipefail; go build ./' + norm(p) + '/... && go vet ./' + norm(p) + '/... && go test -count=1 ./' + norm(p) +
      '/...`; if `git diff -- ' + p + '` changes a function that writes a file, also `go test -count=1 ./internal/schemas/`. green=true only if every command exited 0. Edit nothing.',
    { phase: 'Gate', label: 'regate:' + p, schema: GATE, model: S.gate.model, effort: S.gate.effort },
  ).catch(() => null)
  const ok = !!(g2 && g2.green)
  d.push('re-gate ' + (ok ? 'green' : 'red') + ' in ' + p)
  return { green: ok, simplify, review, fixall: { fixed: findings.length, status: 'done' }, decisions: d }
}

// Estimate: 2 finders + ~4 rederivers per package, plus 3 per package that gets fixed.
const SLICE = Number.isInteger(a.slice) && a.slice > 0 ? a.slice : 0
const ALL = PKGS
if (SLICE) PKGS = ALL.slice(0, SLICE)
log('relay3: ' + PKGS.length + ' package(s), est. ' + (PKGS.length * 6) + ' agents' + (FIX ? ' (+3 per fixed package, up to ' + (PKGS.length * 3) + ', +4 more with polish' : '') +
  (SLICE && ALL.length > SLICE ? '; slice=' + SLICE + ' skipped ' + (ALL.length - SLICE) + ': ' + ALL.slice(SLICE).join(', ') : ''))
const results = (await pipeline(PKGS, review, repair)).filter(Boolean)
// Findings cited outside their finder's package: fix them per owning directory after the stream, one at a time.
const foreign = {}
for (const r of results) for (const c of r.confirmed) if (!inPkg(c.file, r.pkg) && ORDER[c.severity] <= MIN_FIX) (foreign[dirOf(norm(c.file))] = foreign[dirOf(norm(c.file))] || []).push(c)
for (const dir of Object.keys(foreign)) {
  if (!FIX) break
  const fr = await repair({ pkg: dir, merged: 0, confirmed: foreign[dir], dropped: [], unverified: [] })
  fr.confirmed = []
  fr.decisions = ['re-routed ' + foreign[dir].length + ' finding(s) cited outside their finder\'s package to ' + dir, ...(fr.decisions || [])]
  results.push(fr)
}
const confirmed = results.flatMap((r) => r.confirmed).sort((x, y) => ORDER[x.severity] - ORDER[y.severity])
const unverified = results.flatMap((r) => r.unverified || [])
const red = results.filter((r) => r.fixed && !r.green).map((r) => r.pkg)
const fixed = results.filter((r) => r.fixed).map((r) => ({ pkg: r.pkg, findings: r.fixed, impl: r.impl, test: r.test, gate: r.gate, simplify: r.simplify, review: r.review, fixall: r.fixall }))
const decisions = results.flatMap((r) => r.decisions || [])
if (!FIX) decisions.push('review only: fix=false')
if (unverified.length) decisions.push(unverified.length + ' finding(s) unverified: rederive agent returned nothing')
if (SLICE && ALL.length > SLICE) decisions.push('slice=' + SLICE + ': skipped ' + (ALL.length - SLICE) + ' package(s)')
const bullets = confirmed.map((c) => '- **' + c.severity + '** `' + c.file + ':' + c.line + '` (' + c.symbol + ') — ' + c.symptom)
const report = ['# relay3: ' + confirmed.length + ' confirmed finding(s) in ' + PKGS.length + ' package(s)', ...bullets,
  '', 'Fixed: ' + fixed.map((f) => f.pkg + ' (' + f.findings + ')').join(', ') + (fixed.length ? '' : 'none') + '. Red: ' + (red.join(', ') || 'none') + '.',
  ...(unverified.length ? ['Unverified (rederive failed): ' + unverified.map((u) => u.file + ':' + u.line).join(', ') + '.'] : [])].join('\n')
return {
  exit: red.length ? 'red' : FIX ? 'green' : 'review',
  route: 'relay3',
  packages: PKGS.length,
  merged: results.reduce((n, r) => n + r.merged, 0),
  confirmed,
  dropped: results.flatMap((r) => r.dropped),
  unverified,
  fixed,
  red,
  decisions,
  report,
}
