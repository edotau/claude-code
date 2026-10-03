export const meta = {
  name: 'audit-fix-verify',
  description: 'Audit a change set: parallel scoped auditors → refute each finding → per-package implementer ‖ test agent → gate loop',
  whenToUse:
    'Post-refactor or pre-merge audits over a commit range (diff lenses) or a whole path set (code lenses: correctness, failure-modes, contracts). ADK shape: Parallel[auditors] → Sequential[refute pipeline, Parallel[fix‖test per package], Loop gate]. args: {range: "main..HEAD" | paths: [...], cwd, packages: ["internal/x", …], dimensions?: [{key, prompt, effort?}], chunkChars?: 50000, gate?, maxIterations?, fix: true, minFix: "medium", findings?: [{file, line, package, title, scenario, evidence, severity, noTest?}]}. findings skips straight to Fix; noTest on every finding in a package skips its test agent. fix:false stops after Refute; lows below minFix are returned as deferred.',
  phases: [
    { title: 'Scope', detail: 'one cheap agent per package group returns per-file sizes; the script packs them into ≤50 KB chunks' },
    { title: 'Find', detail: 'one auditor per (dimension × chunk) reading its chunk in-context — agent count scales with diff size' },
    { title: 'Refute', detail: 'one skeptic per package batch (≤6 findings); re-anchors wrong citations, defaults to refuted when the evidence is thin' },
    { title: 'Fix', detail: 'per package: implementer (Opus only for highs, handed the evidence) ‖ test agent' },
    { title: 'Gate', detail: 'gate-loop workflow over the touched packages' },
  ],
}

// ── Args ────────────────────────────────────────────────────────────────────
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const CWD = a.cwd || ''
const IN = CWD ? ' in ' + CWD : ''
const CD = CWD ? 'cd ' + CWD + ' && ' : ''
const RANGE = a.range || ''
const PATHS = Array.isArray(a.paths) ? a.paths.filter(Boolean) : []
const PKGS = Array.isArray(a.packages) && a.packages.length ? a.packages : PATHS
// A saved workflow is imported to read its meta, so a missing arg must not throw at module top level.
// A pre-verified findings list (same shape as FINDINGS_SCHEMA items) skips Scope/Find/Refute and goes straight to Fix.
const PRE = Array.isArray(a.findings) && a.findings.length ? a.findings : null
if (!RANGE && !PATHS.length && !PRE) return { error: 'audit-fix-verify needs args.range, args.paths or args.findings' }
const FIX = a.fix !== false
const MAX_ITER = a.maxIterations || 3
// Severity floor for the Fix phase: lows are reported, not fixed (round 1 spent 3 agents on one low). Set minFix:'low' to fix all.
const ORDER = { critical: 0, high: 1, medium: 2, low: 3 }
const MIN_FIX = ORDER[a.minFix] !== undefined ? ORDER[a.minFix] : ORDER.medium
const TARGET = RANGE ? 'the diff of ' + RANGE : 'the code under ' + PATHS.join(', ')
const SHARED_TREE =
  'Shared live tree: never git stash|checkout|reset|restore|clean|commit, never make build|install|test|audit, never edit settings.json. ' +
  (CWD ? 'Prefix every shell command with `cd ' + CWD + ' &&`. ' : '') + 'Comments 1 line ≤120 chars.'

const DEFAULT_DIMENSIONS = [
  { key: 'seams', prompt: 'dangling seams: callers of deleted or renamed symbols in comments, help text, registry rows, completion tables, retired-word lists; flags parsed but never read; fields declared but never set' },
  { key: 'regressions', prompt: 'behaviour regressions: a path that used to check or emit X and silently no longer does; a doc or dry-run surface pointing at a retired verb; a test rewritten to pin the broken output' },
  { key: 'tests', prompt: 'tests deleted alongside kept code, tests skipped in the range, guard tests that pin filenames the change moved, goldens that no longer match', effort: 'low' },
]
// Files mode (paths, no range) audits whole files, so the lenses are about the CODE, not the change.
const CODE_DIMENSIONS = [
  { key: 'correctness', prompt: 'logic defects: wrong conditionals, off-by-one, nil/empty/zero-value paths that produce a wrong result or a crash, error values checked then ignored' },
  { key: 'failure-modes', prompt: 'error handling: swallowed errors, fail-open where the comment or name promises fail-closed, exec/HTTP without a timeout or context, locks that can be skipped, partial writes' },
  { key: 'contracts', prompt: 'contract drift: help text, comments or docs that disagree with the code; flags parsed but never read; fields declared but never set; exported symbols with no caller (verify with grep)', effort: 'low', hitsOnly: true },
]
const FILES_MODE = !RANGE && PATHS.length > 0
const DIMENSIONS = Array.isArray(a.dimensions) && a.dimensions.length ? a.dimensions : (FILES_MODE ? CODE_DIMENSIONS : DEFAULT_DIMENSIONS)
// Group packages ≤4 per auditor so one context holds the whole group's diff.
const GROUPS = []
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

// ── Scope: capture each group's diff ONCE (round 1: auditors spent 31–113 turns each rediscovering it) ──
phase('Scope')
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
const scopes = PRE ? [] : await parallel(GROUPS.map((g, gi) => () =>
  agent(
    FILES_MODE
      ? `${CD}run ONE command: find ${g.join(' ')} -name '*.go' ! -name '*_test.go' -print0 | xargs -0 wc -c — and return each file as an entry ` +
        `with path and bytes. Do not read the files; the auditors cat their own chunk. At most 2 tool calls.`
      : `${CD}run ONE command: git diff --numstat ${RANGE || 'HEAD~1..HEAD'} -- ${g.join(' ') || '.'} — and return each file as an entry with path ` +
        `(the new path for a rename) and bytes = (added + deleted) * 80 (binary "-" counts as 0). Do not read the diff; the auditors run it for their chunk. At most 2 tool calls.`,
    { label: `scope:${gi}`, phase: 'Scope', schema: SCOPE_SCHEMA, model: 'sonnet', effort: 'low' }
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
  else if (!scopes[gi]) chunks.push({ group: g, gi, files: [], chars: 0 }) // scope failed: the auditor falls back to git itself
  log(`scope ${gi}: ${files.length} files, ~${files.reduce((n, f) => n + (f.bytes || 0), 0)} bytes`)
})
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
let refuteCalls = 0
let auditorCount = 0
const relPath = p => (CWD ? String(p || '').replace(CWD + '/', '') : String(p || '')).replace(/^\.\//, '')
const REFUTE_BATCH = a.refuteBatch || 6
const findStage = u => {
  auditorCount++
  return agent(
    `Read-only audit of ${TARGET}${IN}. Packages: ${u.group.join(', ') || 'all touched'}. Lens: ${u.dim.prompt}.\n` +
    (FILES_MODE
      ? `First tool call: cat the files listed below (one call, all of them). Judge from that; open another file only to check a caller or a test (budget 8 tool calls). `
      : `First tool call: ${CD}git diff ${RANGE || 'HEAD~1..HEAD'} -- <the files listed below> (one call, all of them). Judge from that diff; open a file only to check a caller or a test named in it (budget 8 tool calls). `) +
    `Report only defects you can evidence with a grep, diff or test run — no style nits. ${SHARED_TREE}\n\n` +
    (u.chunk.files.length
      ? `Files: ${u.chunk.files.map(f => f.path).join(' ')}\n\n`
      : `(the scope agent returned nothing — ${FILES_MODE ? 'read the files under ' + u.group.join(' ') : 'run git diff --stat ' + (RANGE || 'HEAD~1..HEAD') + ' -- ' + (u.group.join(' ') || '.')} yourself)`),
    { label: `find:${u.key}`, phase: 'Find', schema: FINDINGS_SCHEMA, model: 'sonnet', effort: u.dim.effort || 'low' }
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
      const k = f.package || relPath(f.file).split('/').slice(0, -1).join('/')
      byPkg.set(k, [...(byPkg.get(k) || []), f])
    }
    const batches = []
    for (const [pkg, all] of byPkg) for (let i = 0; i < all.length; i += REFUTE_BATCH) batches.push([pkg, all.slice(i, i + REFUTE_BATCH), i / REFUTE_BATCH])
    return Promise.all(batches.map(([pkg, fs, bi]) => {
      refuteCalls++
      const list = fs.map((f, i) => `${i}. ${f.title}\n   At: ${f.file}:${f.line}\n   Scenario: ${f.scenario}\n   Evidence: ${f.evidence}`).join('\n')
      return agent(
        `Try to REFUTE each of these ${fs.length} audit finding(s) in ${pkg} against the real tree${IN} (read-only); one verdict per index.\n${list}\n\n` +
        `Refuted means: the code path does not do what is claimed, the scenario cannot occur, or it is already handled; quote the line that decides it. ` +
        `Default to refuted=true when the evidence does not hold up. A TRUE claim with a WRONG file or line is NOT refuted: set refuted=false and give the correct file and line. ` +
        // A "no such verb" finding once survived and its fixer rewrote a correct doc; the verb was an alias spelling.
        `A claim that a verb, flag, symbol or file does NOT exist is refuted unless a tree-wide grep (alias and registry tables included) AND running the built binary both come back empty.`,
        { label: `refute:${pkg.split('/').pop()}:${bi}`, phase: 'Refute', schema: VERDICTS_SCHEMA, model: 'sonnet', effort: 'medium' }
      ).then(v => {
        const verdicts = v && Array.isArray(v.verdicts) ? v.verdicts : []
        return fs.map((f, i) => {
          const vd = verdicts.find(x => x.index === i)
          if (!vd || vd.refuted) { refuted.push({ ...f, reason: vd ? vd.reason : 'no verdict' }); return null }
          return { ...f, file: vd.file || f.file, line: vd.line || f.line, lens: u.dim.key }
        })
      })
    })).then(groups => groups.flat())
}
const collect = results => results.filter(Boolean).flat().filter(Boolean)
const confirmed = PRE ? PRE.map(f => ({ ...f })) : collect(await pipeline(mkUnits(primaryDims, chunks, ''), findStage, refuteStage))
if (!PRE && hitDims.length) {
  const flagged = new Set(confirmed.map(f => relPath(f.file)))
  const hitChunks = chunks.map(c => ({ ...c, files: c.files.filter(f => flagged.has(relPath(f.path))) })).filter(c => c.files.length)
  log(`hits-only pass: ${hitChunks.length} chunk(s) over ${flagged.size} flagged file(s)`)
  if (hitChunks.length) confirmed.push(...collect(await pipeline(mkUnits(hitDims, hitChunks, 'h'), findStage, refuteStage)))
}
confirmed.sort((x, y) => ORDER[x.severity] - ORDER[y.severity])
const toFix = confirmed.filter(f => ORDER[f.severity] <= MIN_FIX)
const deferred = confirmed.filter(f => ORDER[f.severity] > MIN_FIX)
const metrics = { scopers: GROUPS.length, chunks: chunks.length, auditors: auditorCount, refuters: refuteCalls, confirmed: confirmed.length, refuted: refuted.length, deferred: deferred.length }
log(`confirmed ${confirmed.length}, refuted ${refuted.length}, below the fix floor ${deferred.length}`)
if (!FIX || !toFix.length) return { confirmed, refuted, deferred, fixed: [], gate: null, metrics }

// ── Fix (ADK ParallelAgent per package: implementer ‖ test agent, disjoint file ownership) ────
phase('Fix')
const byPkg = new Map()
for (const f of toFix) byPkg.set(f.package, [...(byPkg.get(f.package) || []), f])
const fixed = await parallel([...byPkg.entries()].map(([pkg, fs]) => () => {
  const list = fs.map(f => `- ${f.file}:${f.line} [${f.severity}] ${f.title}\n  scenario: ${f.scenario}\n  evidence: ${f.evidence}`).join('\n')
  const files = [...new Set(fs.map(f => relPath(f.file)))]
  const ctx = `Confirmed defects in ${pkg}${IN}:\n${list}\n${SHARED_TREE}`
  // Rounds 9–10: fixers were 65% of cache reads (~45 turns each) re-deriving what the audit already proved.
  // Opus only where a high/critical sits; the evidence rides in the prompt and the files are read in ONE call.
  const hot = fs.some(f => ORDER[f.severity] <= ORDER.high)
  const testsNeeded = fs.some(f => relPath(f.file).endsWith('.go') && !f.noTest) // docs-only or all-noTest: no test agent
  // Implementer ‖ test agent in the same dispatch (rules/standards/quality.md), never test-after.
  return parallel([
    () => agent(`${ctx}\nYou are the IMPLEMENTER: fix every listed defect in non-test files of ${pkg} only; never *_test.go or testdata/. ` +
      `The evidence above is already verified — do not re-audit. First tool call: cat ${files.join(' ')} (one call). ` +
      `Budget ~15 tool calls; then gofmt -l + go vet the package and report per defect.`,
      { label: `fix:${pkg}`, phase: 'Fix', schema: RESULT_SCHEMA, model: hot ? 'opus' : 'sonnet', effort: hot ? 'high' : 'medium' }),
    () => testsNeeded ? agent(`${ctx}\nYou are the TEST agent (an implementer is fixing the non-test files concurrently): *_test.go and testdata/ in ${pkg} only. ` +
      `Read the cited files and their _test.go files in one call; repair tests the fixes break, add one regression test per defect pinning its scenario ` +
      `(none for a comment-only or dead-code fix), run only ./${pkg}/... once the implementer's edits land, and report. Budget ~20 tool calls.`,
      { label: `test:${pkg}`, phase: 'Fix', schema: RESULT_SCHEMA, model: 'sonnet', effort: 'medium' }) : null,
  ]).then(([impl, tests]) => ({ package: pkg, impl, tests }))
}))

// ── Gate (ADK LoopAgent via the saved gate-loop workflow) ────────────────────────────────────
phase('Gate')
const pkgs = [...byPkg.keys()]
// Only packages with a .go finding are Go packages: vetting rules/harness (markdown) failed round 1 of 2026-09-17.
const goPkgs = pkgs.filter(p => byPkg.get(p).some(f => relPath(f.file).endsWith('.go')))
const goArgs = goPkgs.map(p => './' + p + '/').join(' ')
const gate = await workflow('gate-loop', {
  gate: a.gate || (goPkgs.length ? `go build ./... && go vet ${goArgs} && go test ${goArgs}` : 'go build ./...'),
  cwd: CWD || undefined, scope: pkgs, maxIterations: MAX_ITER,
})

return { confirmed, refuted, deferred, fixed: fixed.filter(Boolean), gate, metrics: { ...metrics, fixPairs: byPkg.size, gateIterations: gate ? gate.iterations : 0 } }
