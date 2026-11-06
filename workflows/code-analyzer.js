export const meta = {
  name: 'code-analyzer',
  description: 'Audit a change set: parallel scoped auditors → refute each finding → per-package implementer, then test agent → gate loop',
  whenToUse:
    'USE: audit before a merge or after a refactor; review-only with fix:false. CALL: Workflow({scriptPath: "~/.claude/workflows/code-analyzer.js", args: {cwd, paths: [...] | range: "main..HEAD" | findings}}). OPTIONAL: fix (default true), minFix, rounds, dimensions (replaces the lenses), tests, steps {scope, audit, refute, fix, test, gate} = {model, effort} per stage (claude only). COST: about 8 agent slots per run, so one run at a time. RETURNS {exit, confirmed, refuted, rounds}. Details: saved-workflows.md.',
  phases: [
    { title: 'Scope', detail: 'one cheap agent per package group returns per-file sizes; the script packs them into ≤50 KB chunks' },
    { title: 'Find', detail: 'one auditor per (dimension × chunk) reading its chunk in-context — agent count scales with diff size' },
    { title: 'Refute', detail: 'one skeptic per package batch (≤6 findings); re-anchors wrong citations, defaults to refuted when the evidence is thin' },
    { title: 'Fix', detail: 'per package: implementer (opus only for highs), then test agent with its result' },
    { title: 'Gate', detail: 'green-keeper-loop workflow over the touched packages, then re-audits what it touched for up to rounds rounds' },
  ],
}

// ── Args ────────────────────────────────────────────────────────────────────
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const CWD = a.cwd || ''
const IN = CWD ? ' in ' + CWD : ''
const CD = CWD ? 'cd ' + CWD + ' && ' : ''
let PATHS = Array.isArray(a.paths) ? a.paths.filter(Boolean) : []
let PKGS = Array.isArray(a.packages) && a.packages.length ? a.packages : PATHS
// A pre-verified findings list (same shape as FINDINGS_SCHEMA items) skips Scope/Find/Refute and goes straight to Fix.
let PRE = Array.isArray(a.findings) && a.findings.length ? a.findings : null
// No range, paths or findings → audit the uncommitted working tree (git diff HEAD).
const RANGE = a.range || (PATHS.length || PRE ? '' : 'HEAD')
const FIX = a.fix !== false
// tests:false skips the per-package test agent for every package (no test agent).
if (a.tests !== undefined && typeof a.tests !== 'boolean') return { exit: 'error', error: 'code-analyzer: args.tests must be a boolean', rounds: [] }
const TESTS = a.tests !== false
const MAX_ITER = Number.isInteger(a.maxIterations) && a.maxIterations > 0 ? a.maxIterations : 3
// Rounds re-audit exactly the non-test source the previous round's fix touched, until dry/stalled/capped (D2: in-script loop, not self-recursion).
const ROUNDS = Number.isInteger(a.rounds) && a.rounds > 0 ? a.rounds : 2
let PRIOR = new Set(Array.isArray(a.prior) ? a.prior : [])
// Clean break: the per-stage args moved into steps.
const REPLACED = { auditModel: 'audit', refuteModel: 'refute', refuters: 'refute', profiles: 'refute' }
for (const k of Object.keys(REPLACED)) {
  if (a[k] !== undefined) return { exit: 'error', error: `code-analyzer: ${k} was replaced by steps.${REPLACED[k]} (plans/2026-11-03-workflow-steps.md)`, rounds: [] }
}
const TEST_GLOBS = '*_test.go, testdata/, test_*.py, *_test.py, tests/, conftest.py, *.test.*, *.spec.*, __tests__/, fixtures/'
const SRC_RE = /\.(go|py|ts|tsx|js|jsx|mjs|rs|java)$/
const TEST_RE = /(_test\.(go|py)$|\.(test|spec)\.[a-z]+$|(^|\/)test_[^/]*\.py$|(^|\/)(tests?|__tests__|testdata|fixtures)\/)/
// Severity floor for the Fix phase: lows are reported, not fixed (round 1 spent 3 agents on one low). Set minFix:'low' to fix all.
const ORDER = { critical: 0, high: 1, medium: 2, low: 3 }
const MIN_FIX = ORDER[a.minFix] !== undefined ? ORDER[a.minFix] : ORDER.medium
let TARGET = RANGE ? 'the diff of ' + RANGE : 'the code under ' + PATHS.join(', ')
const SHARED_TREE =
  'Shared live tree: never git stash|checkout|reset|restore|clean|commit, never make build|install|test|audit, never edit settings.json. ' +
  (CWD ? 'Prefix every shell command with `cd ' + CWD + ' &&`. ' : '') + 'Comments 1 line ≤120 chars.'

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

// ── Steps: who runs each stage; defaults reproduce the pre-steps models and efforts ──
const STAGES = ['scope', 'audit', 'refute', 'fix', 'test', 'gate']
const OPT = { allowed: STAGES }
const RAW = a.steps && typeof a.steps === 'object' && !Array.isArray(a.steps) ? a.steps : {}
let S
try {
  S = {
    scope: stepOf(a.steps, 'scope', { model: 'haiku', effort: 'low' }, OPT),
    audit: stepOf(a.steps, 'audit', { model: 'sonnet', effort: 'low' }, OPT),
    refute: stepOf(a.steps, 'refute', { model: 'sonnet', effort: 'medium' }, OPT),
    fix: stepOf(a.steps, 'fix', { model: 'sonnet', effort: 'medium' }, OPT),
    test: stepOf(a.steps, 'test', { model: 'sonnet', effort: 'medium' }, OPT),
    gate: stepOf(a.steps, 'gate', {}, OPT),
  }
} catch (e) { return { exit: 'error', error: 'code-analyzer: ' + e.message, rounds: [] } }
// Audit effort stays per lens unless steps.audit.effort is set; fix keeps opus+high for highs unless set.
const AUDIT_EFFORT = RAW.audit && RAW.audit.effort
const FIX_MODEL = RAW.fix && RAW.fix.model
const FIX_EFFORT = RAW.fix && RAW.fix.effort

// ── Pressure: Claude account state read once; low/exhausted steps every Claude stage down one model ──
const routeState = await agent(
  `Run exactly: ${CD}claude-code gateway router --format json --agent claude --fallback none --model sonnet. ` +
    `It prints one JSON object; state = candidates[0].state when candidates is non-empty, else the skipped[] entry whose agent ` +
    `is "claude" and its gate value, else "unknown". Never run anything else.`,
  { label: 'route-state', phase: 'Scope', schema: { type: 'object', additionalProperties: false, required: ['state'], properties: { state: { type: 'string' } } }, model: 'haiku', effort: 'low' }
).catch(() => null)
const MODEL_DOWN = { opus: 'sonnet', sonnet: 'haiku', haiku: 'haiku' }
const PRESSURE = !!(routeState && (routeState.state === 'low' || routeState.state === 'exhausted'))
const routeModel = m => (PRESSURE && MODEL_DOWN[m]) ? MODEL_DOWN[m] : m
if (PRESSURE) log(`pressure: claude account ${routeState.state} — stepping Claude stages down one model`)
else if (!routeState) log('pressure: route read failed — no downshift (fail open)')
// Passed to every child so none re-reads it; a failed read passes 'unknown' (no downshift anywhere).
const CLAUDE_STATE = routeState && typeof routeState.state === 'string' ? routeState.state : 'unknown'

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
let FILES_MODE = !RANGE && PATHS.length > 0
let DIMENSIONS = Array.isArray(a.dimensions) && a.dimensions.length ? a.dimensions : (FILES_MODE ? CODE_DIMENSIONS : DEFAULT_DIMENSIONS)
// Group packages/files ≤4 per auditor so one context holds the whole group.
let GROUPS = []
for (let i = 0; i < Math.max(1, PKGS.length); i += 4) GROUPS.push(PKGS.slice(i, i + 4))

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
        required: ['file', 'line', 'package', 'title', 'scenario', 'evidence', 'severity'],
        properties: {
          file: { type: 'string', description: 'repo-relative path' },
          line: { type: 'integer' },
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
const RESULT_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['status', 'filesChanged', 'summary'], // leftUndone optional: an agent with nothing left omitted it and burned 5 retries
  properties: {
    status: { type: 'string', enum: ['done', 'partial', 'blocked'] },
    filesChanged: { type: 'array', items: { type: 'string' } },
    summary: { type: 'string' },
    leftUndone: { type: 'array', items: { type: 'string' } },
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
const norm = f => ({
  ...f,
  package: f.package || relPath(f.file).split('/').slice(0, -1).join('/') || '.',
  severity: ORDER[f.severity] === undefined ? (log(`unknown severity '${f.severity}' on ${f.file} → medium`), 'medium') : f.severity,
})
const key = f => relPath(f.file) + ':' + f.title
const REFUTE_BATCH = a.refuteBatch || 6

// ── Round loop (D2: workflow() nesting is one level only, so recursion is an in-script for-loop) ──
const rounds = []
for (let round = 1; round <= ROUNDS; round++) {
  // ── Scope: capture each group's diff/file sizes ONCE per round (round 1: auditors spent 31–113 turns rediscovering it) ──
  phase('Scope')
  const scopes = PRE ? [] : await parallel(GROUPS.map((g, gi) => () =>
    agent(
      FILES_MODE
        ? `${CD}run ONE command: find ${g.join(' ')} -type f \\( -name '*.go' -o -name '*.py' -o -name '*.ts' -o -name '*.tsx' -o -name '*.js' -o -name '*.jsx' -o -name '*.mjs' -o -name '*.rs' -o -name '*.java' \\) ! -path '*/node_modules/*' ! -path '*/.venv/*' ! -path '*/vendor/*' ! -path '*/.git/*' ! -name '*_test.go' ! -name '*_test.py' ! -name 'test_*.py' ! -name '*.test.*' ! -name '*.spec.*' ! -path '*/tests/*' ! -path '*/__tests__/*' ! -path '*/testdata/*' -print0 | xargs -0 wc -c — and return each file as an entry ` +
          `with path and bytes. Do not read the files; the auditors cat their own chunk. At most 2 tool calls.`
        : `${CD}run ONE command: git diff --numstat ${RANGE || 'HEAD~1..HEAD'} -- ${g.join(' ') || '.'} — and return each file as an entry with path ` +
          `(the new path for a rename) and bytes = (added + deleted) * 80 (binary "-" counts as 0). Do not read the diff; the auditors run it for their chunk. At most 2 tool calls.`,
      { label: `scope:${round}:${gi}`, phase: 'Scope', schema: SCOPE_SCHEMA, model: routeModel(S.scope.model), effort: S.scope.effort } // file listing: schema-checked, haiku is enough
    )
  ))
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
  if (!PRE && FILES_MODE && !chunks.length) {
    rounds.push({ error: 'code-analyzer: no source files under ' + PATHS.join(', '), exit: 'error', round })
    break
  }
  log(`${chunks.length} chunk(s) of ≤${CHUNK_CHARS} chars → ${DIMENSIONS.filter(d => !d.hitsOnly).length * chunks.length} first-pass auditors`)

  // ── Find → Refute as ONE pipeline: a chunk's findings are refuted while other chunks still audit ──
  // hitsOnly lenses (contracts) run in a second pass over just the files the first pass confirmed a finding in:
  // round 9–10 measured contracts at a third of Find while yielding mostly lows.
  const mkUnits = (dims, cs, pass) => dims.flatMap(d => cs.map((c, ci) => ({ dim: d, group: c.group, key: `${d.key}:${pass}${ci}`, chunk: c })))
  const primaryDims = DIMENSIONS.filter(d => !d.hitsOnly)
  const hitDims = DIMENSIONS.filter(d => d.hitsOnly)
  log('auditing ' + TARGET + ' — ' + primaryDims.length + ' lens(es) × ' + chunks.length + ' chunk(s)' + (hitDims.length ? ', then ' + hitDims.map(d => d.key).join('/') + ' over flagged files' : ''))
  const seen = new Set()
  const refuted = []
  const unverified = []
  let refuteCalls = 0
  let auditorCount = 0
  const findStage = u => {
    auditorCount++
    const prompt =
      `Read-only audit of ${TARGET}${IN}. Packages: ${u.group.join(', ') || 'all touched'}. Lens: ${u.dim.prompt}.\n` +
      (FILES_MODE
        ? `First tool call: cat the files listed below (one call, all of them). Judge from that; open another file only to check a caller or a test (budget 8 tool calls). `
        : `First tool call: ${CD}git diff ${RANGE || 'HEAD~1..HEAD'} -- <the files listed below> (one call, all of them). Judge from that diff; open a file only to check a caller or a test named in it (budget 8 tool calls). `) +
      `Report only defects you can evidence with a grep, diff or test run — no style nits. ${SHARED_TREE}\n` +
      `Method: ~/.claude/skills/evaluate-code/SKILL.md (report only findings on changed code you verified; >80% confidence filter)\n\n` +
      (u.chunk.files.length
        ? `Files: ${u.chunk.files.map(f => f.path).join(' ')}\n\n`
        : `(the scope agent returned nothing — ${FILES_MODE ? 'read the files under ' + u.group.join(' ') : 'run git diff --stat ' + (RANGE || 'HEAD~1..HEAD') + ' -- ' + (u.group.join(' ') || '.')} yourself)`)
    return agent(prompt,
      { label: `find:${round}:${u.key}`, phase: 'Find', schema: FINDINGS_SCHEMA, model: routeModel(S.audit.model), effort: AUDIT_EFFORT || u.dim.effort || 'low' }
    )
  }
  const refuteStage = (found, u) => {
    const fresh = (found ? found.findings : []).filter(f => {
      const k = `${f.file}:${f.line}:${f.title}`
      if (seen.has(k)) return false
      seen.add(k)
      return true
    })
    // Batched per PACKAGE, ≤ REFUTE_BATCH findings each (round 11: per-file refuters grew to 24 agents / 7.9M reads).
    const byPkg = new Map()
    for (const f of fresh) {
      const k = f.package || relPath(f.file).split('/').slice(0, -1).join('/') || '.'
      byPkg.set(k, [...(byPkg.get(k) || []), f])
    }
    const batches = []
    for (const [pkg, all] of byPkg) for (let i = 0; i < all.length; i += REFUTE_BATCH) batches.push([pkg, all.slice(i, i + REFUTE_BATCH), i / REFUTE_BATCH])
    return Promise.all(batches.map(([pkg, fs, bi]) => {
      refuteCalls++
      const list = fs.map((f, i) => `${i}. ${f.title}\n   At: ${f.file}:${f.line}\n   Scenario: ${f.scenario}\n   Evidence: ${f.evidence}`).join('\n')
      const brief = `Try to REFUTE each of these ${fs.length} audit finding(s) in ${pkg} against the real tree${IN} (read-only); one verdict per index.\n${list}\n\n` +
        `Refuted means: the code path does not do what is claimed, the scenario cannot occur, or it is already handled; quote the line that decides it. ` +
        `Default to refuted=true when the evidence does not hold up. A TRUE claim with a WRONG file or line is NOT refuted: set refuted=false and give the correct file and line. ` +
        // A "no such verb" finding once survived and its fixer rewrote a correct doc; the verb was an alias spelling.
        `A claim that a verb, flag, symbol or file does NOT exist is refuted unless a tree-wide grep (alias, registration and config tables included) AND the repo's own test or CLI output both come back empty.`
      return agent(brief, { label: `refute:${round}:${pkg.split('/').pop()}:${bi}`, phase: 'Refute', schema: VERDICTS_SCHEMA, model: routeModel(S.refute.model), effort: S.refute.effort }).then(v => {
        const verdicts = v && Array.isArray(v.verdicts) ? v.verdicts : []
        return fs.map((f, i) => {
          if (!v) { unverified.push({ ...f, reason: 'refuter returned no result' }); return null }
          const vd = verdicts.find(x => x.index === i)
          if (!vd || vd.refuted) { refuted.push({ ...f, reason: vd ? vd.reason : 'no verdict' }); return null }
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
  confirmed.sort((x, y) => ORDER[x.severity] - ORDER[y.severity])
  const toFix = confirmed.filter(f => ORDER[f.severity] <= MIN_FIX)
  const deferred = confirmed.filter(f => ORDER[f.severity] > MIN_FIX)
  const metrics = { scopers: PRE ? 0 : GROUPS.length, chunks: chunks.length, auditors: auditorCount, refuters: refuteCalls, confirmed: confirmed.length, refuted: refuted.length, deferred: deferred.length }
  log(`round ${round}/${ROUNDS}: confirmed ${confirmed.length}, refuted ${refuted.length}, below the fix floor ${deferred.length}, unverified ${unverified.length}`)

  const base = { confirmed, refuted, unverified, deferred, round }
  if (!FIX || !toFix.length) { rounds.push({ ...base, fixed: [], gate: null, exit: FIX ? 'dry' : 'no-fix', metrics }); break }
  // Same findings as an earlier round (PRIOR, accumulated across rounds): report, do not re-fix forever.
  if (PRIOR.size && toFix.every(f => PRIOR.has(key(f)))) { rounds.push({ ...base, fixed: [], gate: null, exit: 'stalled', metrics }); break }

  // ── Fix (packages in parallel; per package ADK Sequential: implementer, then test agent; disjoint file ownership) ────
  phase('Fix')
  const byPkg = new Map()
  for (const f of toFix) byPkg.set(f.package, [...(byPkg.get(f.package) || []), f])
  const fixed = await parallel([...byPkg.entries()].map(([pkg, fs]) => () => {
    const list = fs.map(f => `- ${f.file}:${f.line} [${f.severity}] ${f.title}\n  scenario: ${f.scenario}\n  evidence: ${f.evidence}`).join('\n')
    const files = [...new Set(fs.map(f => relPath(f.file)))]
    const ctx = `Confirmed defects in ${pkg}${IN}:\n${list}\n${SHARED_TREE}`
    // Opus only where a high/critical sits; the evidence rides in the prompt and the files are read in ONE call.
    const hot = fs.some(f => ORDER[f.severity] <= ORDER.high)
    const testsNeeded = TESTS && fs.some(f => !f.noTest && SRC_RE.test(relPath(f.file))) // docs-only or all-noTest: no test agent
    // Test agent runs after the implementer and gets its result, so it asserts against code that has landed.
    return (async () => {
      const implTask = `${ctx}\nYou are the IMPLEMENTER: fix every listed defect in non-test files of ${pkg} only; never test files or fixtures (${TEST_GLOBS}). ` +
        `The evidence above is already verified — do not re-audit. First tool call: cat ${files.join(' ')} (one call). ` +
        `Budget ~15 tool calls; then run the repo's formatter and linter on the files you touched (whichever it has: make fmt/lint, gofmt + go vet, ruff, eslint) and report per defect.\n` +
        `Before a behavior change write TASK: code does X; check expects Y; spec says Z (Y vs Z disagree = stop, report).\n` +
        `After a defect fix write ISSUE: searched P, found N.\n` +
        `Method: ~/.claude/skills/problem-solving/SKILL.md (Steps 4-6)\n` +
        `Put the TASK/ISSUE facts as plain sentences in your summary.`
      const impl = await agent(implTask,
        { label: `fix:${round}:${pkg}`, phase: 'Fix', schema: RESULT_SCHEMA, agentType: 'code-worker:claude', model: routeModel(FIX_MODEL || (hot ? 'opus' : 'sonnet')), effort: FIX_EFFORT || (hot ? 'high' : 'medium') })
      const implBlocked = !impl || impl.status === 'blocked'
      const testTask = () => `${ctx}\nImplementer result (already landed on disk): status ${impl.status}; files changed: ${(impl.filesChanged || []).join(', ') || 'none'}; summary: ${impl.summary}\nYou are the TEST agent (the implementer has finished): only test files and fixtures (${TEST_GLOBS}) for ${pkg}. ` +
        `Method: ~/.claude/agents/copilot.md → TEST. ` +
        `Read the cited files and their tests in one call; repair tests the fixes break, add one regression test per defect pinning its scenario ` +
        `(none for a comment-only or dead-code fix), run only the tests for ${pkg}, and report. Budget ~20 tool calls.`
      const tests = !testsNeeded || implBlocked ? null : await agent(testTask(),
        { label: `test:${round}:${pkg}`, phase: 'Fix', schema: RESULT_SCHEMA, agentType: 'code-worker:claude', model: routeModel(S.test.model), effort: S.test.effort })
      return { package: pkg, impl, tests, blocked: implBlocked }
    })()
  })).then(rs => rs.map((r, i) => r || { package: [...byPkg.keys()][i], blocked: true, error: 'fix stage failed' }))

  // ── Gate (ADK LoopAgent via the saved green-keeper-loop workflow; a.gate passes through — green-keeper-loop auto-detects) ──
  phase('Gate')
  const pkgs = [...byPkg.keys()]
  const gate = await workflow('green-keeper-loop', { claudeState: CLAUDE_STATE, gate: a.gate, cwd: CWD || undefined, scope: pkgs, maxIterations: MAX_ITER, ...(RAW.gate ? { steps: { fix: RAW.gate } } : {}) })

  const metricsOut = { ...metrics, fixPairs: byPkg.size, gateIterations: gate ? gate.iterations : 0 }
  const out = { ...base, fixed, gate, metrics: metricsOut }
  if (!gate || gate.status !== 'green' || out.fixed.some(x => x.blocked)) { rounds.push({ ...out, exit: 'blocked' }); break }
  const touched = [...new Set([
    ...out.fixed.flatMap(x => [...((x.impl && x.impl.filesChanged) || []), ...((x.tests && x.tests.filesChanged) || [])]),
    ...(gate.filesChanged || []),
  ].map(relPath))].filter(p => SRC_RE.test(p) && !TEST_RE.test(p))
  if (!touched.length) { rounds.push({ ...out, exit: 'nothing-changed' }); break }
  if (round >= ROUNDS) { rounds.push({ ...out, exit: 'capped' }); break }
  log(`round ${round + 1}/${ROUNDS}: re-auditing ${touched.length} file(s) the fix touched`)
  rounds.push(out)
  // Next round audits exactly what this round's fix touched — PKGS/GROUPS are just the touched file list, chunked.
  PRIOR = new Set([...PRIOR, ...toFix.map(key)])
  PATHS = touched
  PKGS = touched
  FILES_MODE = true
  GROUPS = []
  for (let i = 0; i < Math.max(1, PKGS.length); i += 4) GROUPS.push(PKGS.slice(i, i + 4))
  DIMENSIONS = Array.isArray(a.dimensions) && a.dimensions.length ? a.dimensions : CODE_DIMENSIONS
  TARGET = 'the code under ' + PATHS.join(', ')
  PRE = null
}

const lastRound = rounds[rounds.length - 1] || { exit: 'error', round: 0 }
return { ...lastRound, exit: lastRound.exit, round: lastRound.round, rounds, unverified: rounds.flatMap(r => r.unverified || []) }
