export const meta = {
  name: 'relay3',
  description: 'Streaming relay: each package flows find → rederive → fix → guard test → gate on its own, no barrier between packages',
  whenToUse:
    'USE: audit and repair several Go packages (or a LOW/MEDIUM job\'s files) without a barrier between them; review-only with fix:false. CALL: Workflow({scriptPath: "~/.claude/workflows/relay3.js", args: {cwd, packages: [...] | paths: [...]}}). OPTIONAL: fix (default true), minFix (default medium), focus, steps {find, rederive, fix, test, gate} = {model, effort} per stage (claude only). RETURNS {exit, route, packages, merged, confirmed, dropped, fixed, red, decisions, report}. Details: saved-workflows.md.',
  phases: [
    { title: 'Find', detail: 'two lenses per package; claims merged by file ±10 lines or same symbol within the package' },
    { title: 'Rederive', detail: 'each claim re-derived blind from file:line + symptom' },
    { title: 'Fix', detail: 'as soon as a package has confirmed findings at or above minFix, one implementer fixes them' },
    { title: 'Test', detail: 'one guard test per fix, proven with claude-code test mutate' },
    { title: 'Gate', detail: 'package-scoped build/vet/test, plus internal/schemas when a writer changed' },
  ],
}

// ── stepOf: copied verbatim into code-analyzer, code-engineer, green-keeper-loop, code-evaluator ──
// stepOf(steps, name, defaults, {allowed}) -> {model, effort}; claude only, any bad input throws Error naming stage/field.
const stepOf = (steps, name, defaults, opt) => {
  const S_CLAUDE = ['haiku', 'sonnet', 'opus']
  const S_EFFORT = ['low', 'medium', 'high', 'xhigh']
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
const PKGS = Array.isArray(a.packages) && a.packages.length ? a.packages.filter(Boolean) : [...new Set(RAWP.map(dirOf))]
if (!CWD || !PKGS.length) return ERR('args.cwd and one of args.packages or args.paths are required')
// ── Steps: who runs each stage ──
const STAGES = ['find', 'rederive', 'fix', 'test', 'gate']
const OPT = { allowed: STAGES }
let S
try {
  S = {
    find: stepOf(a.steps, 'find', { model: 'sonnet', effort: 'medium' }, OPT),
    rederive: stepOf(a.steps, 'rederive', { model: 'sonnet', effort: 'medium' }, OPT),
    fix: stepOf(a.steps, 'fix', { model: 'sonnet', effort: 'medium' }, OPT),
    test: stepOf(a.steps, 'test', { model: 'sonnet', effort: 'medium' }, OPT),
    gate: stepOf(a.steps, 'gate', { model: 'haiku', effort: 'low' }, OPT),
  }
} catch (e) { return ERR(e.message) }
const FIX = a.fix !== false
const ORDER = { critical: 0, high: 1, medium: 2, low: 3 }
const MIN_FIX = ORDER[a.minFix] !== undefined ? ORDER[a.minFix] : ORDER.medium
const NEAR = 10
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
  const ok = judged.filter(Boolean)
  const confirmed = ok.filter((x) => x.r && x.r.reproduced).map((x) => ({ ...x.f, severity: x.r.severity, rederived: x.r.scenario }))
  const dropped = ok.filter((x) => !x.r || !x.r.reproduced).map((x) => ({ file: x.f.file, line: x.f.line, symptom: x.f.symptom }))
  return { pkg: p, merged: merged.length, confirmed, dropped }
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
  const green = !!(gate && gate.green)
  const decisions = ['fixed ' + cs.length + ' finding(s) at or above minFix in ' + p, green ? 'gate green in ' + p : 'gate red in ' + p]
  return { ...rev, fixed: cs.length, impl, test, gate, decisions }
}

const results = (await pipeline(PKGS, review, repair)).filter(Boolean)
// Findings cited outside their finder's package: fix them per owning directory after the stream, one at a time.
const foreign = {}
for (const r of results) for (const c of r.confirmed) if (!inPkg(c.file, r.pkg) && ORDER[c.severity] <= MIN_FIX) (foreign[dirOf(norm(c.file))] = foreign[dirOf(norm(c.file))] || []).push(c)
for (const dir of Object.keys(foreign)) {
  if (!FIX) break
  const fr = await repair({ pkg: dir, merged: 0, confirmed: foreign[dir], dropped: [] })
  fr.confirmed = []
  fr.decisions = ['re-routed ' + foreign[dir].length + ' finding(s) cited outside their finder\'s package to ' + dir, ...(fr.decisions || [])]
  results.push(fr)
}
const confirmed = results.flatMap((r) => r.confirmed).sort((x, y) => ORDER[x.severity] - ORDER[y.severity])
const red = results.filter((r) => r.fixed && !(r.gate && r.gate.green)).map((r) => r.pkg)
const fixed = results.filter((r) => r.fixed).map((r) => ({ pkg: r.pkg, findings: r.fixed, impl: r.impl, test: r.test, gate: r.gate }))
const decisions = results.flatMap((r) => r.decisions || [])
if (!FIX) decisions.push('review only: fix=false')
const bullets = confirmed.map((c) => '- **' + c.severity + '** `' + c.file + ':' + c.line + '` (' + c.symbol + ') — ' + c.symptom)
const report = ['# relay3: ' + confirmed.length + ' confirmed finding(s) in ' + PKGS.length + ' package(s)', ...bullets,
  '', 'Fixed: ' + fixed.map((f) => f.pkg + ' (' + f.findings + ')').join(', ') + (fixed.length ? '' : 'none') + '. Red: ' + (red.join(', ') || 'none') + '.'].join('\n')
return {
  exit: red.length ? 'red' : FIX ? 'green' : 'review',
  route: 'relay3',
  packages: PKGS.length,
  merged: results.reduce((n, r) => n + r.merged, 0),
  confirmed,
  dropped: results.flatMap((r) => r.dropped),
  fixed,
  red,
  decisions,
  report,
}
