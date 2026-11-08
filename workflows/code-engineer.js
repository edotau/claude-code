export const meta = {
  name: 'code-engineer',
  description: 'One size-routed entry point: size the job, then route it through code-workers (or draft a plan for an epic)',
  whenToUse:
    'USE: any code job when you do not want to pick a workflow; sizes it (TRIVIAL/LOW/MEDIUM/HIGH/EPIC) and routes. CALL: Workflow({scriptPath: "~/.claude/workflows/code-engineer.js", args: {cwd, task?, packages?, paths?, range?}}) with at least one of task/packages/paths/range (range = a git range like main..HEAD; its changed Go files become paths). OPTIONAL: tier (skips sizing), fix (default true), minFix (default low: every finding is fixed, decision #54), focus, label (report file name, default code-engineer), record (default true; false skips the Record step), steps {size, intel, impl, plan, dedupe, simplify, review, record, child} = {agent?, model?, effort?, profile?, provider?} per stage (agent claude|codex|copilot|gemini|opencode; external stages run on the session\'s route, or args/steps profile|provider, and fall back on 429/auth; each reports ran on or BLOCKED). OPTIONAL: profile, provider (run-wide override for external stages). child is code-workers\'s steps, where Simplify, Review and Fix-all also run per package. polish (default true; false skips both levels: code-workers\'s per-package chain and code-engineer\'s final pass) runs after code-workers when it fixed something: Dedupe (only when 2+ packages were fixed), then Simplify and Review over the merged diff; any finding, lows included, earns one more code-workers pass (capped at one loop). fast (default true; false disables): a LOW/MEDIUM fix task passes its files to code-workers as known, skipping Find and Rederive. A low token budget (under 150k left) skips the final pass, the review loop and code-workers\'s per-package polish. RETURNS {exit, tier, route, packages, merged, confirmed, dropped, unverified, fixed, red, files (sorted repo-relative files the implementer, fixers and test writers changed), dedupe, simplify, review, decisions, report}. Details: saved-workflows.md.',
  phases: [
    { title: 'Size', detail: 'one cheap agent reads the task, git status/diff and go list and returns the tier, packages and paths' },
    { title: 'Route', detail: 'MEDIUM adds an intel agent, a feature task adds an implementer, EPIC drafts a plan and stops' },
    { title: 'Run', detail: 'code-workers over the paths (TRIVIAL/LOW/MEDIUM) or the packages (HIGH)' },
    { title: 'Dedupe', detail: 'when code-workers fixed 2+ packages, one agent consolidates helpers duplicated across their combined diff (polish:false skips)' },
    { title: 'Simplify', detail: 'one agent runs review simplify over the merged diff of all fixed files and applies the simplify gate' },
    { title: 'Review', detail: 'one read-only agent reviews the merged diff; any finding (lows included) earns one more code-workers pass' },
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

const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const CWD = a.cwd || ''
const TASK = a.task ? String(a.task) : ''
const ERR = (m, tier, route, decisions) => ({ exit: 'error', error: 'code-engineer: ' + m, tier: tier || '', route: route || '', decisions: decisions || [], report: '' })
const strs = (v) => (Array.isArray(v) ? v.filter(Boolean).map(String) : [])
let PATHS = strs(a.paths)
let PKGS = strs(a.packages)
const TIERS = ['TRIVIAL', 'LOW', 'MEDIUM', 'HIGH', 'EPIC']
const RANGE = a.range ? String(a.range) : ''
if (RANGE && !/^[A-Za-z0-9._\/~^@{}-]+(\.\.\.?[A-Za-z0-9._\/~^@{}-]+)?$/.test(RANGE)) return ERR('args.range must be a git range like main..HEAD')
if (!CWD || !(TASK || PATHS.length || PKGS.length || RANGE)) return ERR('args.cwd and one of args.task, args.packages, args.paths or args.range are required')
if (a.tier !== undefined && !TIERS.includes(a.tier)) return ERR('args.tier must be ' + TIERS.join('|'))
// steps.child is code-workers's own steps; the rest are ours.
const { child: CHILD_RAW, ...OWN } = a.steps && typeof a.steps === 'object' && !Array.isArray(a.steps) ? a.steps : {}
// a fix task never runs impl (code-workers' fix stage does the edit), so steps.impl drives that fix stage unless child.fix is set
const CHILD_STEPS = OWN.impl && !(CHILD_RAW && CHILD_RAW.fix) ? { ...(CHILD_RAW || {}), fix: OWN.impl } : CHILD_RAW
const OPT = { external: true, profile: a.profile, provider: a.provider, allowed: ['size', 'intel', 'impl', 'plan', 'dedupe', 'simplify', 'review', 'record'] }
let S
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
const WORK = { type: 'object', properties: { status: { type: 'string', enum: ['done', 'partial', 'blocked'] }, summary: { type: 'string' }, files: { type: 'array', items: { type: 'string' } } }, required: ['status', 'summary'] }
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
const DEDUPE = {
  type: 'object',
  properties: { applied: { type: 'array', items: { type: 'string' } }, flagged: { type: 'array', items: { type: 'string' } }, deltaLines: { type: 'integer' }, status: { type: 'string', enum: ['done', 'clean', 'partial', 'blocked'] } },
  required: ['applied', 'flagged', 'deltaLines', 'status'],
}
// lowBudget: an optional extra pass is skipped when a budget is set and under 150k tokens remain.
const lowBudget = () => typeof budget !== 'undefined' && !!budget && !!budget.total && budget.remaining() < 150000
const RECORDED = { type: 'object', properties: { paths: { type: 'array', items: { type: 'string' } } }, required: ['paths'] }

// ── Size ──
phase('Size')
const decisions = []
let tier = a.tier
let focus = a.focus || TASK
let KIND = ''
// A git range becomes the changed Go files (non-test), so a diff audit needs no separate workflow.
if (RANGE) {
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
if (!tier && !TASK) {
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
const route = tier === 'EPIC' ? 'plan' : tier === 'HIGH' ? 'code-workers:packages' : 'code-workers:paths'
decisions.push('route: ' + route)
log('route: ' + route + ' (tier ' + tier + ', kind ' + (KIND || 'n/a') + ')')
const head = '# code-engineer: ' + tier + ' → ' + route
phase('Route')
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
let IMPL_FILES = []
if ((tier === 'LOW' || tier === 'MEDIUM') && TASK && target.paths.length && feature) {
  const impl = S.impl.done('impl', await agent(
    S.impl.task(RULES + ' Make this change in these files (new files in the same package ok) and nothing else; smallest correct change, functions < 50 lines, 1-line comments. ' +
      'Run gofmt and go vet on the package. Do not write tests; code-workers follows with find, fix, guard test and gate. Return `files`: every repo-relative file you changed.\nFiles: ' + target.paths.join(', ') + '\nTask: ' + TASK + '\nContext: ' + focus),
    { phase: 'Route', label: 'impl', schema: S.impl.schema(WORK), model: S.impl.model, effort: S.impl.effort, ...S.impl.type },
  ).catch(() => null))
  if (impl) IMPL_FILES = strs(impl.files).map((f) => f.replace(CWD + '/', ''))
  decisions.push('implementer ran: ' + (impl ? impl.status + ' — ' + impl.summary : 'returned nothing'))
  decisions.push('feature task: code-workers below is an audit-and-harden pass over the new code, not a feature builder')
}

// ── Run ──
phase('Run')
let r
// Fast path: a LOW/MEDIUM fix task names the bug, so code-workers skips Find and Rederive for those files (audits and features never).
const fastFiles = (tier === 'LOW' || tier === 'MEDIUM') && KIND === 'fix' && TASK && a.fix !== false && a.fast !== false ? (sized.length ? sized : target.paths || []).filter((p) => /\.go$/.test(p) && !/_test\.go$/.test(p)) : []
const known = fastFiles.map((file) => ({ file, symptom: TASK }))
if (known.length) { decisions.push('fast path: ' + known.length + ' file(s) passed to code-workers as known'); log('fast path: known ' + known.length + ' file(s)') }
try {
  r = await workflow({ scriptPath: CWD + '/workflows/code-workers.js' }, { cwd: CWD, ...target, fix: a.fix, minFix: a.minFix, focus, steps: CHILD_STEPS, profile: a.profile, provider: a.provider, polish: a.polish, ...(known.length ? { known } : {}) })
} catch (e) { return ERR('code-workers threw: ' + e.message, tier, route, decisions) }
if (!r || r.exit === 'error') return ERR((r && r.error) || 'code-workers returned nothing', tier, route, decisions)
let res = {
  exit: r.exit, tier, route, packages: r.packages, merged: r.merged, confirmed: r.confirmed, dropped: r.dropped, unverified: r.unverified || [], fixed: r.fixed, red: r.red,
  decisions: [...decisions, ...(r.decisions || [])], report: head + '\n' + (r.report || ''),
}
const wantPolish = a.polish !== false && a.fix !== false && (r.fixed || []).length
if (wantPolish && lowBudget()) {
  res.decisions.push('polish skipped: budget under 150k tokens remaining')
  log('polish skipped: low budget')
} else if (wantPolish) res = await polish(res)
return await record(res)

// touchedFiles is the sorted, deduped union of the files the implementer, fixers and test writers reported changing.
function touchedFiles(fixed) {
  const norm = (f) => String(f).replace(CWD + '/', '')
  const fs = (fixed || []).flatMap((f) => [...strs(f.impl && f.impl.files), ...strs(f.test && f.test.files)])
  return [...new Set([...IMPL_FILES, ...fs.map(norm)])].sort()
}

// changedFiles lists the files code-workers's fixers and test writers reported, repo-relative; a package dir stands in when none were named.
function changedFiles(fixed) {
  const norm = (f) => String(f).replace(CWD + '/', '')
  const files = fixed.flatMap((f) => [...strs(f.impl && f.impl.files), ...strs(f.test && f.test.files)].map(norm))
  return [...new Set(files.length ? files : fixed.map((f) => norm(f.pkg)))]
}

// polish: Dedupe (2+ packages), Simplify over the merged diff, Review; any finding, lows included, earns one more code-workers pass.
async function polish(res) {
  const files = changedFiles(res.fixed)
  const scope = 'Only these files (the tree is shared and has other dirty files; never touch or diff others): ' + files.join(' ') + '. Diff: `git diff -- ' + files.join(' ') + '`.'
  const dd = res.fixed.length >= 2 ? await dedupe(res, files) : null
  const pre = dd ? dd.line : 'dedupe skipped: code-workers fixed ' + res.fixed.length + ' package'
  phase('Simplify')
  const sp = S.simplify.done('simplify', await agent(
    S.simplify.task(RULES + ' Simplify gate (' + CWD + '/skills/simplify/SKILL.md, quality.md -> Simplify), run ONCE over the merged diff. ' + scope +
      ' Run `claude-code review simplify ' + files.join(' ') + '`, then one in-context pass (reuse, simplification, efficiency, altitude). Name the assumption before every cut; ' +
      'flag instead of cutting when it has no covering test; never cut semantic comments, error context or distinct tests; never fix bugs (flag them). Edit only the listed non-test files. ' +
      'After edits run gofmt, go vet and go test for the touched packages; if a test regresses, revert your own edit by hand (never git checkout/restore/stash) and flag it. ' +
      'Return applied[] and flagged[] as `file:line · what · assumption`, deltaLines (net changed lines, negative = removed) and status (clean = nothing to do).'),
    { phase: 'Simplify', label: 'simplify', schema: S.simplify.schema(SIMP), model: S.simplify.model, effort: S.simplify.effort, ...S.simplify.type },
  ).catch(() => null))
  const simplify = sp || { applied: [], flagged: [], deltaLines: 0, status: 'blocked' }
  const d = [pre, 'simplify: ' + (sp ? simplify.applied.length + ' applied, ' + simplify.flagged.length + ' flagged, Δ ' + simplify.deltaLines + ' lines (' + simplify.status + ')' : 'agent returned nothing')]
  phase('Review')
  const rv = S.review.done('review', await agent(
    S.review.task(RULES + ' Read-only: edit nothing, never run go test. Review the final diff with the ' + CWD + '/skills/evaluate-code/SKILL.md correctness and contract lenses (wrong results, broken invariants, ' +
      'changed behavior callers rely on, errors dropped). ' + scope + ' Each finding: file, line, symbol, one-line symptom, severity. Return {findings: []} when clean.'),
    { phase: 'Review', label: 'review', schema: S.review.schema(REVF), model: S.review.model, effort: S.review.effort, ...S.review.type },
  ).catch(() => null))
  const findings = rv ? rv.findings : []
  const sev = ['critical', 'high', 'medium', 'low'].map((k) => [k, findings.filter((f) => f.severity === k).length]).filter((x) => x[1])
  const review = { findings, ran: !!rv }
  d.push('review: ' + (rv ? findings.length + ' findings (' + (sev.map((x) => x[1] + ' ' + x[0]).join(', ') || 'none') + ')' : 'agent returned nothing'))
  const hot = findings
  log('polish: simplify ' + simplify.applied.length + ' applied/' + simplify.flagged.length + ' flagged, review ' + findings.length + ' finding(s), ' + hot.length + ' to fix')
  const out = { ...res, dedupe: dd ? dd.out : undefined, simplify, review }
  if (hot.length && lowBudget()) {
    d.push('review loop skipped: budget under 150k tokens remaining')
    log('review loop skipped: low budget')
  } else if (hot.length) {
    const hotPaths = [...new Set(hot.map((f) => String(f.file).replace(CWD + '/', '')).filter((f) => /\.go$/.test(f) && !/_test\.go$/.test(f)))]
    const text = hot.map((f) => f.file + ':' + f.line + ' (' + f.symbol + ', ' + f.severity + '): ' + f.symptom).join('; ')
    let r2 = null
    if (hotPaths.length) r2 = await workflow({ scriptPath: CWD + '/workflows/code-workers.js' }, { cwd: CWD, paths: hotPaths, fix: true, minFix: 'low', fast: true, known: hot.filter((f) => hotPaths.includes(String(f.file).replace(CWD + '/', ''))).map((f) => ({ file: String(f.file).replace(CWD + '/', ''), symptom: f.symptom })), focus: 'Review findings to fix, all severities: ' + text, steps: CHILD_STEPS, profile: a.profile, provider: a.provider, polish: false }).catch(() => null)
    d.push('review loop: ' + (r2 && r2.exit !== 'error' ? 'ran once over ' + hotPaths.join(', ') + ' (capped at one loop)' : 'skipped, no usable Go paths or code-workers failed'))
    if (r2 && r2.exit !== 'error') {
      out.exit = r2.exit === 'red' || res.red.length ? 'red' : res.exit
      out.fixed = [...res.fixed, ...(r2.fixed || [])]
      out.red = [...res.red, ...(r2.red || [])]
      d.push(...(r2.decisions || []))
    }
  } else d.push('review loop: skipped, no finding')
  const sec = ['', '## Simplify', simplify.status + ', Δ ' + simplify.deltaLines + ' lines', ...simplify.applied.map((x) => '- applied: ' + x), ...simplify.flagged.map((x) => '- flagged: ' + x),
    '', '## Review', findings.length ? findings.map((f) => '- **' + f.severity + '** `' + f.file + ':' + f.line + '` (' + f.symbol + ') — ' + f.symptom).join('\n') : 'no findings']
  return { ...out, decisions: [...res.decisions, ...d], report: res.report + (dd ? dd.report : '') + '\n' + sec.join('\n') }
}

// dedupe runs over the combined diff of 2+ fixed packages and looks only for helpers duplicated across them.
async function dedupe(res, files) {
  const pkgs = res.fixed.map((f) => String(f.pkg).replace(CWD + '/', ''))
  phase('Dedupe')
  const dd = S.dedupe.done('dedupe', await agent(
    S.dedupe.task(RULES + ' Cross-package dedupe, the reuse angle of ' + CWD + '/skills/simplify/SKILL.md, over the combined diff of parallel fixes. Only these files (the tree is shared; never touch others): ' +
      files.join(' ') + '. Diff: `git diff -- ' + files.join(' ') + '`. Look ONLY for helpers or logic duplicated across these packages (grep siblings, `git log -S`); compare a near-duplicate ' +
      'with its original for behavior the copy lost, then consolidate at the original (callers import it; no new shim). Name the assumption before every cut; flag when no test covers it; never fix bugs (flag them). ' +
      'Afterwards run gofmt, go vet and go test for every affected package (' + pkgs.join(', ') + '); if a test regresses, revert your own edit by hand (never git checkout/restore/stash) and flag it. ' +
      'Return applied[] and flagged[] as `file:line · what · assumption`, deltaLines (net changed lines, negative = removed) and status (clean = nothing to do).'),
    { phase: 'Dedupe', label: 'dedupe', schema: S.dedupe.schema(DEDUPE), model: S.dedupe.model, effort: S.dedupe.effort, ...S.dedupe.type },
  ).catch(() => null))
  const out = dd || { applied: [], flagged: [], deltaLines: 0, status: 'blocked' }
  const line = 'dedupe: ' + (dd ? out.applied.length + ' applied, ' + out.flagged.length + ' flagged, Δ ' + out.deltaLines + ' lines (' + out.status + ')' : 'agent returned nothing')
  log(line)
  const sec = ['', '## Dedupe', out.status + ', Δ ' + out.deltaLines + ' lines', ...out.applied.map((x) => '- applied: ' + x), ...out.flagged.map((x) => '- flagged: ' + x)]
  return { out, line, report: '\n' + sec.join('\n') }
}

// record appends the run's report and decisions to the repo's memory reports so continuity needs no hand-written notes.
async function record(res0) {
  const files = touchedFiles(res0.fixed)
  const res = { ...res0, files, report: res0.report + (files.length ? '\n\nFiles touched: ' + files.join(', ') : '') }
  if (a.record === false) return { ...res, coworkers: COWORKERS }
  const label = String(a.label || 'code-engineer').replace(/[^A-Za-z0-9._-]+/g, '-')
  const recordOnce = () => agent(
    S.record.task(RULES + ' Write two files and nothing else. Run `cd ' + CWD + ' && claude-code memory path` for the store dir D and `date +%F` for DATE. ' +
      '(1) Append (create if missing) the markdown REPORT below, followed by one blank line, to D/reports/DATE-' + label + '.md. ' +
      '(2) Append each DECISION below as a line `- DATE · ' + label + ' · <decision>` to D/reports/DATE-decisions.md (create with a `# Decisions` title). ' +
      'Then run `cd ' + CWD + ' && claude-code memory fmt` so both files pass the docs lint. Never edit any other file. Return the two paths.\nREPORT:\n' + res.report + '\nDECISIONS:\n' + res.decisions.map((d) => '- ' + d).join('\n')),
    { phase: 'Record', label: 'record', schema: S.record.schema(RECORDED), model: S.record.model, effort: S.record.effort, ...S.record.type },
  ).catch(() => null)
  let w = S.record.done('record', await recordOnce())
  if (!w || !(w.paths || []).length) w = S.record.done('record', await recordOnce()) // a no-op record (paths []) gets one retry
  return { ...res, coworkers: COWORKERS, recorded: w ? w.paths : [], decisions: [...res.decisions, w ? 'recorded: ' + w.paths.join(', ') : 'record agent returned nothing'] }
}
