export const meta = {
  name: 'orchestrate',
  description: 'One size-routed entry point: size the job, then route it through relay3 (or draft a plan for an epic)',
  whenToUse:
    'USE: any code job when you do not want to pick a workflow; sizes it (TRIVIAL/LOW/MEDIUM/HIGH/EPIC) and routes. CALL: Workflow({scriptPath: "~/.claude/workflows/orchestrate.js", args: {cwd, task?, packages?, paths?}}) with at least one of task/packages/paths. OPTIONAL: tier (skips sizing), fix (default true), minFix (default medium), focus, steps {size, intel, impl, plan, child} = {model, effort} per stage (claude only; child is relay3\'s steps). RETURNS {exit, tier, route, packages, merged, confirmed, dropped, unverified, fixed, red, decisions, report}. Details: saved-workflows.md.',
  phases: [
    { title: 'Size', detail: 'one cheap agent reads the task, git status/diff and go list and returns the tier, packages and paths' },
    { title: 'Route', detail: 'MEDIUM adds an intel agent, a feature task adds an implementer, EPIC drafts a plan and stops' },
    { title: 'Run', detail: 'relay3 over the paths (TRIVIAL/LOW/MEDIUM) or the packages (HIGH)' },
    { title: 'Record', detail: 'one cheap agent appends the report and decisions to the memory store (record:false skips)' },
  ],
}

// ── stepOf: copied verbatim into code-analyzer, code-engineer, green-keeper-loop, code-evaluator ──
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
if (!CWD || !(TASK || PATHS.length || PKGS.length)) return ERR('args.cwd and one of args.task, args.packages or args.paths are required')
if (a.tier !== undefined && !TIERS.includes(a.tier)) return ERR('args.tier must be ' + TIERS.join('|'))
// steps.child is relay3's own steps; the rest are ours.
const { child: CHILD_STEPS, ...OWN } = a.steps && typeof a.steps === 'object' && !Array.isArray(a.steps) ? a.steps : {}
const OPT = { allowed: ['size', 'intel', 'impl', 'plan', 'record'] }
let S
try {
  S = {
    size: stepOf(OWN, 'size', { model: 'haiku', effort: 'low' }, OPT),
    intel: stepOf(OWN, 'intel', { model: 'sonnet', effort: 'medium' }, OPT),
    impl: stepOf(OWN, 'impl', { model: 'sonnet', effort: 'medium' }, OPT),
    plan: stepOf(OWN, 'plan', { model: 'sonnet', effort: 'medium' }, OPT),
    record: stepOf(OWN, 'record', { model: 'haiku', effort: 'low' }, OPT),
  }
} catch (e) { return ERR(e.message) }

const RULES =
  'Shared live tree: never git stash|checkout|reset|restore|clean|commit|add, never make build|install|audit, never edit settings.json. ' +
  'Prefix every shell command with `cd ' + CWD + ' &&`.'
const SIZED = {
  type: 'object',
  properties: { tier: { type: 'string', enum: TIERS }, packages: { type: 'array', items: { type: 'string' } }, paths: { type: 'array', items: { type: 'string' } }, focus: { type: 'string' }, reason: { type: 'string' } },
  required: ['tier', 'packages', 'paths', 'focus', 'reason'],
}
const INTEL = { type: 'object', properties: { paths: { type: 'array', items: { type: 'string' } }, focus: { type: 'string' } }, required: ['paths', 'focus'] }
const WORK = { type: 'object', properties: { status: { type: 'string', enum: ['done', 'partial', 'blocked'] }, summary: { type: 'string' } }, required: ['status', 'summary'] }
const PLAN = { type: 'object', properties: { plan: { type: 'string' } }, required: ['plan'] }
const RECORDED = { type: 'object', properties: { paths: { type: 'array', items: { type: 'string' } } }, required: ['paths'] }

// ── Size ──
phase('Size')
const decisions = []
let tier = a.tier
let focus = a.focus || TASK
if (!tier && !TASK) {
  tier = PKGS.length ? 'HIGH' : 'LOW'
  decisions.push('sized as ' + tier + ': no task, ' + (PKGS.length ? 'a package list is an audit' : 'only named paths'))
} else if (!tier) {
  const sz = await agent(
    RULES + ' Read-only: edit nothing. Size this job by the tiers TRIVIAL (one-line fix), LOW (one function or file), MEDIUM (2-5 files or needs tests, one module), ' +
      'HIGH (cross-module, 5+ files, or an audit of packages), EPIC (multi-session, architectural). Audit requests are HIGH. Use `git status --short`, ' +
      '`git diff --stat` and `go list ./...` only as needed. Return the Go package dirs (repo-relative, e.g. internal/notices) and the files the task names ' +
      '(repo-relative; directories ok), plus a one-line focus and the reason for the tier.\nTask: ' + TASK + (PKGS.length ? '\nGiven packages: ' + PKGS.join(', ') : '') + (PATHS.length ? '\nGiven paths: ' + PATHS.join(', ') : ''),
    { phase: 'Size', label: 'size', schema: SIZED, model: S.size.model, effort: S.size.effort },
  ).catch(() => null)
  if (!sz) return ERR('sizing agent returned nothing; pass args.tier to skip it')
  tier = sz.tier
  decisions.push('sized as ' + tier + ': ' + sz.reason)
  if (!PKGS.length) PKGS = strs(sz.packages)
  if (!PATHS.length) PATHS = strs(sz.paths)
  if (!a.focus && sz.focus) focus = sz.focus
} else decisions.push('sized as ' + tier + ': args.tier given')

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
if ((tier === 'LOW' || tier === 'MEDIUM') && TASK && target.paths.length && /\b(build|add|implement|create|introduce|support|write)\b/i.test(TASK) && !/\b(audit|review)\b/i.test(TASK)) {
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
return await record({
  exit: r.exit, tier, route, packages: r.packages, merged: r.merged, confirmed: r.confirmed, dropped: r.dropped, unverified: r.unverified || [], fixed: r.fixed, red: r.red,
  decisions: [...decisions, ...(r.decisions || [])], report: head + '\n' + (r.report || ''),
})

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
