export const meta = {
  name: 'orchestrate',
  description: 'One size-routed entry point: size the job, then route it through relay3 (or draft a plan for an epic)',
  whenToUse:
    'USE: any code job when you do not want to pick a workflow; sizes it (TRIVIAL/LOW/MEDIUM/HIGH/EPIC) and routes. CALL: Workflow({scriptPath: "~/.claude/workflows/orchestrate.js", args: {cwd, task?, packages?, paths?, range?}}) with at least one of task/packages/paths/range (range = a git range like main..HEAD; its changed Go files become paths). OPTIONAL: tier (skips sizing), fix (default true), minFix (default medium), focus, steps {size, intel, impl, plan, child} = {model, effort} per stage (claude only; child is relay3\'s steps). polish (default true; false skips Simplify and Review) runs once after relay3 when it fixed something; steps also take simplify, review. RETURNS {exit, tier, route, packages, merged, confirmed, dropped, unverified, fixed, red, simplify, review, decisions, report}. Details: saved-workflows.md.',
  phases: [
    { title: 'Size', detail: 'one cheap agent reads the task, git status/diff and go list and returns the tier, packages and paths' },
    { title: 'Route', detail: 'MEDIUM adds an intel agent, a feature task adds an implementer, EPIC drafts a plan and stops' },
    { title: 'Run', detail: 'relay3 over the paths (TRIVIAL/LOW/MEDIUM) or the packages (HIGH)' },
    { title: 'Simplify', detail: 'one agent runs review simplify over the files relay3 fixed and applies the simplify gate (polish:false skips)' },
    { title: 'Review', detail: 'one read-only agent reviews that diff; critical/high/medium findings earn one more relay3 pass' },
    { title: 'Record', detail: 'one cheap agent appends the report and decisions to the memory store (record:false skips)' },
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

const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const CWD = a.cwd || ''
const TASK = a.task ? String(a.task) : ''
const ERR = (m, tier, route, decisions) => ({ exit: 'error', error: 'orchestrate: ' + m, tier: tier || '', route: route || '', decisions: decisions || [], report: '' })
const strs = (v) => (Array.isArray(v) ? v.filter(Boolean).map(String) : [])
let PATHS = strs(a.paths)
let PKGS = strs(a.packages)
const TIERS = ['TRIVIAL', 'LOW', 'MEDIUM', 'HIGH', 'EPIC']
const RANGE = a.range ? String(a.range) : ''
if (RANGE && !/^[A-Za-z0-9._\/~^@{}-]+(\.\.\.?[A-Za-z0-9._\/~^@{}-]+)?$/.test(RANGE)) return ERR('args.range must be a git range like main..HEAD')
if (!CWD || !(TASK || PATHS.length || PKGS.length || RANGE)) return ERR('args.cwd and one of args.task, args.packages, args.paths or args.range are required')
if (a.tier !== undefined && !TIERS.includes(a.tier)) return ERR('args.tier must be ' + TIERS.join('|'))
// steps.child is relay3's own steps; the rest are ours.
const { child: CHILD_STEPS, ...OWN } = a.steps && typeof a.steps === 'object' && !Array.isArray(a.steps) ? a.steps : {}
const OPT = { allowed: ['size', 'intel', 'impl', 'plan', 'simplify', 'review', 'record'] }
let S
try {
  S = {
    size: stepOf(OWN, 'size', { model: 'haiku', effort: 'low' }, OPT),
    intel: stepOf(OWN, 'intel', { model: 'sonnet', effort: 'medium' }, OPT),
    impl: stepOf(OWN, 'impl', { model: 'sonnet', effort: 'medium' }, OPT),
    plan: stepOf(OWN, 'plan', { model: 'sonnet', effort: 'medium' }, OPT),
    simplify: stepOf(OWN, 'simplify', { model: 'sonnet', effort: 'medium' }, OPT),
    review: stepOf(OWN, 'review', { model: 'sonnet', effort: 'high' }, OPT),
    record: stepOf(OWN, 'record', { model: 'haiku', effort: 'low' }, OPT),
  }
} catch (e) { return ERR(e.message) }

const RULES =
  'Shared live tree: never git stash|checkout|reset|restore|clean|commit|add, never make build|install|audit, never edit settings.json. ' +
  'Prefix every shell command with `cd ' + CWD + ' &&`.'
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
const WORK = { type: 'object', properties: { status: { type: 'string', enum: ['done', 'partial', 'blocked'] }, summary: { type: 'string' } }, required: ['status', 'summary'] }
const PLAN = { type: 'object', properties: { plan: { type: 'string' } }, required: ['plan'] }
const SIMP = {
  type: 'object',
  properties: { applied: { type: 'array', items: { type: 'string' } }, flagged: { type: 'array', items: { type: 'string' } }, deltaLines: { type: 'integer' }, status: { type: 'string', enum: ['done', 'clean', 'partial', 'blocked'] } },
  required: ['applied', 'flagged', 'deltaLines', 'status'],
}
const REVF = {
  type: 'object',
  properties: { findings: { type: 'array', items: { type: 'object', properties: { file: { type: 'string' }, line: { type: 'integer' }, symbol: { type: 'string' }, symptom: { type: 'string' }, severity: { type: 'string', enum: ['critical', 'high', 'medium', 'low'] } }, required: ['file', 'line', 'symbol', 'symptom', 'severity'] } } },
  required: ['findings'],
}
const RECORDED = { type: 'object', properties: { paths: { type: 'array', items: { type: 'string' } } }, required: ['paths'] }

// ── Size ──
phase('Size')
const decisions = []
let tier = a.tier
let focus = a.focus || TASK
let KIND = ''
// A git range becomes the changed Go files (non-test), so a diff audit needs no separate workflow.
if (RANGE) {
  const ch = await agent(
    RULES + ' Read-only. Run `git diff --name-only ' + RANGE + " -- '*.go'` and return the files that still exist and are not *_test.go, " +
      'repo-relative. Return {paths: []} when there are none.',
    { phase: 'Size', label: 'range', schema: RECORDED, model: S.size.model, effort: S.size.effort },
  ).catch(() => null)
  const got = ch ? strs(ch.paths) : []
  PATHS = [...new Set([...PATHS, ...got])]
  decisions.push('range ' + RANGE + ': ' + got.length + ' changed Go file(s)')
  if (!PATHS.length && !PKGS.length && !TASK) return await record({ exit: 'green', tier: 'TRIVIAL', route: 'none', packages: 0, merged: 0, confirmed: [], dropped: [], unverified: [], fixed: [], red: [], decisions, report: '# orchestrate: range ' + RANGE + ' has no Go changes' })
  if (!tier && !TASK) {
    const dirs = new Set(PATHS.map((p) => p.split('/').slice(0, -1).join('/')))
    tier = dirs.size > 1 || PATHS.length > 5 ? 'HIGH' : 'LOW'
    if (tier === 'HIGH' && !PKGS.length) PKGS = [...dirs]
    decisions.push('sized as ' + tier + ': range spans ' + dirs.size + ' package(s), ' + PATHS.length + ' file(s)')
  }
}
if (!tier && !TASK) {
  tier = PKGS.length ? 'HIGH' : 'LOW'
  decisions.push('sized as ' + tier + ': no task, ' + (PKGS.length ? 'a package list is an audit' : 'only named paths'))
} else if (!tier) {
  const sz = await agent(
    RULES + ' Read-only: edit nothing. Do NOT pick a size; find facts. Read the code the task points at and return: kind (fix = repair ' +
      'existing behavior, feature = add behavior, audit = review packages for bugs, architecture = a multi-session design change); the ' +
      'non-test source FILES that must change (repo-relative, one entry per file, never a directory, tests excluded); the Go package dirs ' +
      'only for an audit; a one-line focus; and a one-line reason. Use `git status --short` and `go list ./...` only as needed.\nTask: ' + TASK +
      (PKGS.length ? '\nGiven packages: ' + PKGS.join(', ') : '') + (PATHS.length ? '\nGiven paths: ' + PATHS.join(', ') : ''),
    { phase: 'Size', label: 'size', schema: SIZED, model: S.size.model, effort: S.size.effort },
  ).catch(() => null)
  if (!sz) return ERR('sizing agent returned nothing; pass args.tier to skip it')
  KIND = sz.kind
  if (!PKGS.length) PKGS = strs(sz.packages)
  if (!PATHS.length) PATHS = strs(sz.paths).filter((p) => !/_test\.go$/.test(p))
  tier = tierOf(sz.kind, PATHS, sz.kind === 'audit' ? PKGS : [])
  decisions.push('sized as ' + tier + ' (rule: ' + sz.kind + ', ' + PATHS.length + ' file(s)): ' + sz.reason)
  if (!a.focus && sz.focus) focus = sz.focus
} else if (a.tier) decisions.push('sized as ' + tier + ': args.tier given')

// ── Route ──
const route = tier === 'EPIC' ? 'plan' : tier === 'HIGH' ? 'relay3:packages' : 'relay3:paths'
decisions.push('route: ' + route)
const head = '# orchestrate: ' + tier + ' → ' + route
phase('Route')
if (tier === 'EPIC') {
  const p = await agent(
    RULES + ' Read-only: edit nothing. Draft an implementation plan for this task in the shape of ' + CWD + '/docs/templates/plan.md (read it first), with the ' +
      'real files and phases found in the repo. Return the markdown in `plan`.\nTask: ' + TASK,
    { phase: 'Route', label: 'plan', schema: PLAN, model: S.plan.model, effort: S.plan.effort },
  ).catch(() => null)
  if (!p) return ERR('plan agent returned nothing', tier, route, decisions)
  decisions.push('epic: no code changed, plan drafted for the owner')
  return await record({ exit: 'plan', tier, route, packages: 0, merged: 0, confirmed: [], dropped: [], unverified: [], fixed: [], red: [], decisions, report: head + '\n' + p.plan })
}
const target = tier === 'HIGH' ? { packages: PKGS.length ? PKGS : [...new Set(PATHS)] } : { paths: PATHS }
if (tier === 'MEDIUM' && TASK) {
  const intel = await agent(
    RULES + ' Read-only: edit nothing. Codebase intel for this task: structure, conventions, and the exact files to change with their callers. ' +
      'Return `paths` (repo-relative files to change or audit) and `focus` (the task plus the conventions and callers that matter).\nTask: ' + TASK +
      (PATHS.length ? '\nStart from: ' + PATHS.join(', ') : ''),
    { phase: 'Route', label: 'intel', schema: INTEL, model: S.intel.model, effort: S.intel.effort },
  ).catch(() => null)
  if (intel && strs(intel.paths).length) { target.paths = strs(intel.paths); focus = TASK + '. Intel: ' + intel.focus } else decisions.push('intel returned nothing; kept the sized paths')
}
// relay3 only fixes bugs: a feature task gets an implementer first, then relay3 audits and hardens the new code.
const feature = KIND ? KIND === 'feature' : /\b(build|add|implement|create|introduce|support|write)\b/i.test(TASK) && !/\b(audit|review)\b/i.test(TASK)
if ((tier === 'LOW' || tier === 'MEDIUM') && TASK && target.paths.length && feature) {
  const impl = await agent(
    RULES + ' Make this change in these files (new files in the same package ok) and nothing else; smallest correct change, functions < 50 lines, 1-line comments. ' +
      'Run gofmt and go vet on the package. Do not write tests; relay3 follows with find, fix, guard test and gate.\nFiles: ' + target.paths.join(', ') + '\nTask: ' + TASK + '\nContext: ' + focus,
    { phase: 'Route', label: 'impl', schema: WORK, model: S.impl.model, effort: S.impl.effort },
  ).catch(() => null)
  decisions.push('implementer ran: ' + (impl ? impl.status + ' — ' + impl.summary : 'returned nothing'))
  decisions.push('feature task: relay3 below is an audit-and-harden pass over the new code, not a feature builder')
}

// ── Run ──
phase('Run')
let r
try {
  r = await workflow({ scriptPath: CWD + '/workflows/relay3.js' }, { cwd: CWD, ...target, fix: a.fix, minFix: a.minFix, focus, steps: CHILD_STEPS })
} catch (e) { return ERR('relay3 threw: ' + e.message, tier, route, decisions) }
if (!r || r.exit === 'error') return ERR((r && r.error) || 'relay3 returned nothing', tier, route, decisions)
let res = {
  exit: r.exit, tier, route, packages: r.packages, merged: r.merged, confirmed: r.confirmed, dropped: r.dropped, unverified: r.unverified || [], fixed: r.fixed, red: r.red,
  decisions: [...decisions, ...(r.decisions || [])], report: head + '\n' + (r.report || ''),
}
if (a.polish !== false && a.fix !== false && (r.fixed || []).length) res = await polish(res, target, focus)
return await record(res)

// changedFiles lists the files relay3's fixers and test writers reported, repo-relative; a package dir stands in when none were named.
function changedFiles(fixed) {
  const norm = (f) => String(f).replace(CWD + '/', '')
  const files = fixed.flatMap((f) => [...strs(f.impl && f.impl.files), ...strs(f.test && f.test.files)].map(norm))
  return [...new Set(files.length ? files : fixed.map((f) => norm(f.pkg)))]
}

// polish runs Simplify once over the merged diff, then Review; a critical/high/medium finding earns one more relay3 pass.
async function polish(res, tgt, foc) {
  const files = changedFiles(res.fixed)
  const scope = 'Only these files (the tree is shared and has other dirty files; never touch or diff others): ' + files.join(' ') + '. Diff: `git diff -- ' + files.join(' ') + '`.'
  phase('Simplify')
  const sp = await agent(
    RULES + ' Simplify gate (' + CWD + '/skills/simplify/SKILL.md, quality.md -> Simplify), run ONCE over the merged diff. ' + scope +
      ' Run `claude-code review simplify ' + files.join(' ') + '`, then one in-context pass (reuse, simplification, efficiency, altitude). Name the assumption before every cut; ' +
      'flag instead of cutting when it has no covering test; never cut semantic comments, error context or distinct tests; never fix bugs (flag them). Edit only the listed non-test files. ' +
      'After edits run gofmt, go vet and go test for the touched packages; if a test regresses, revert your own edit by hand (never git checkout/restore/stash) and flag it. ' +
      'Return applied[] and flagged[] as `file:line · what · assumption`, deltaLines (net changed lines, negative = removed) and status (clean = nothing to do).',
    { phase: 'Simplify', label: 'simplify', schema: SIMP, model: S.simplify.model, effort: S.simplify.effort },
  ).catch(() => null)
  const simplify = sp || { applied: [], flagged: [], deltaLines: 0, status: 'blocked' }
  const d = ['simplify: ' + (sp ? simplify.applied.length + ' applied, ' + simplify.flagged.length + ' flagged, Δ ' + simplify.deltaLines + ' lines (' + simplify.status + ')' : 'agent returned nothing')]
  phase('Review')
  const rv = await agent(
    RULES + ' Read-only: edit nothing, never run go test. Review the final diff with the ' + CWD + '/skills/evaluate-code/SKILL.md correctness and contract lenses (wrong results, broken invariants, ' +
      'changed behavior callers rely on, errors dropped). ' + scope + ' Each finding: file, line, symbol, one-line symptom, severity. Return {findings: []} when clean.',
    { phase: 'Review', label: 'review', schema: REVF, model: S.review.model, effort: S.review.effort },
  ).catch(() => null)
  const findings = rv ? rv.findings : []
  const sev = ['critical', 'high', 'medium', 'low'].map((k) => [k, findings.filter((f) => f.severity === k).length]).filter((x) => x[1])
  const review = { findings, ran: !!rv }
  d.push('review: ' + (rv ? findings.length + ' findings (' + (sev.map((x) => x[1] + ' ' + x[0]).join(', ') || 'none') + ')' : 'agent returned nothing'))
  const hot = findings.filter((f) => f.severity !== 'low')
  const out = { ...res, simplify, review }
  if (hot.length) {
    const hotPaths = [...new Set(hot.map((f) => String(f.file).replace(CWD + '/', '')).filter((f) => /\.go$/.test(f) && !/_test\.go$/.test(f)))]
    const text = hot.map((f) => f.file + ':' + f.line + ' (' + f.symbol + ', ' + f.severity + '): ' + f.symptom).join('; ')
    let r2 = null
    if (hotPaths.length) r2 = await workflow({ scriptPath: CWD + '/workflows/relay3.js' }, { cwd: CWD, paths: hotPaths, fix: true, minFix: a.minFix, focus: 'Review findings to verify and fix: ' + text, steps: CHILD_STEPS }).catch(() => null)
    d.push('review loop: ' + (r2 && r2.exit !== 'error' ? 'ran once over ' + hotPaths.join(', ') + ' (capped at one loop)' : 'skipped, no usable Go paths or relay3 failed'))
    if (r2 && r2.exit !== 'error') {
      out.exit = r2.exit === 'red' || res.red.length ? 'red' : res.exit
      out.fixed = [...res.fixed, ...(r2.fixed || [])]
      out.red = [...res.red, ...(r2.red || [])]
      d.push(...(r2.decisions || []))
    }
  } else d.push('review loop: skipped, no critical/high/medium finding')
  const lows = findings.filter((f) => f.severity === 'low')
  const sec = ['', '## Simplify', simplify.status + ', Δ ' + simplify.deltaLines + ' lines', ...simplify.applied.map((x) => '- applied: ' + x), ...simplify.flagged.map((x) => '- flagged: ' + x),
    '', '## Review', findings.length ? findings.map((f) => '- **' + f.severity + '** `' + f.file + ':' + f.line + '` (' + f.symbol + ') — ' + f.symptom).join('\n') : 'no findings']
  if (lows.length) sec.push('Lows are report-only.')
  return { ...out, decisions: [...res.decisions, ...d], report: res.report + '\n' + sec.join('\n') }
}

// record appends the run's report and decisions to the repo's memory reports so continuity needs no hand-written notes.
async function record(res) {
  if (a.record === false) return res
  const label = String(a.label || 'orchestrate').replace(/[^A-Za-z0-9._-]+/g, '-')
  const w = await agent(
    RULES + ' Write two files and nothing else. Run `cd ' + CWD + ' && claude-code memory path` for the store dir D and `date +%F` for DATE. ' +
      '(1) Append (create if missing) the markdown REPORT below, followed by one blank line, to D/reports/DATE-' + label + '.md. ' +
      '(2) Append each DECISION below as a line `- DATE · ' + label + ' · <decision>` to D/reports/DATE-decisions.md (create with a `# Decisions` title). ' +
      'Never edit any other file. Return the two paths.\nREPORT:\n' + res.report + '\nDECISIONS:\n' + res.decisions.map((d) => '- ' + d).join('\n'),
    { phase: 'Record', label: 'record', schema: RECORDED, model: S.record.model, effort: S.record.effort },
  ).catch(() => null)
  return { ...res, recorded: w ? w.paths : [], decisions: [...res.decisions, w ? 'recorded: ' + w.paths.join(', ') : 'record agent returned nothing'] }
}
