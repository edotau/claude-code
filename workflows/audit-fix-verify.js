export const meta = {
  name: 'audit-fix-verify',
  description: 'Audit a change set: parallel scoped auditors → refute each finding → per-package implementer ‖ test agent → gate loop',
  whenToUse:
    'Post-refactor or pre-merge audits over a commit range or path set. ADK shape: Parallel[auditors] → Sequential[refute pipeline, Parallel[fix‖test per package], Loop gate]. ' +
    'args: {range: "main..HEAD" | paths: [...], cwd, packages: ["internal/x", …], dimensions?: [{key, prompt}], gate?, maxIterations?, fix: true}. fix:false stops after Refute and returns confirmed findings only.',
  phases: [
    { title: 'Find', detail: 'one auditor per (dimension × package group), read-only, structured findings' },
    { title: 'Refute', detail: 'one skeptic per finding, defaults to refuted when the evidence is thin' },
    { title: 'Fix', detail: 'per package: implementer (non-test files) ‖ test agent (test files)' },
    { title: 'Gate', detail: 'gate-loop workflow over the touched packages' },
  ],
}

// ── Args ────────────────────────────────────────────────────────────────────
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const CWD = a.cwd || ''
const WHERE = CWD ? ` in ${CWD}` : ''
const RANGE = a.range || ''
const PATHS = Array.isArray(a.paths) ? a.paths.filter(Boolean) : []
const PKGS = Array.isArray(a.packages) && a.packages.length ? a.packages : PATHS
if (!RANGE && !PATHS.length) throw new Error('audit-fix-verify needs args.range or args.paths')
const FIX = a.fix !== false
const MAX_ITER = a.maxIterations || 3
const TARGET = RANGE ? `the diff \`git diff ${RANGE}\`` : `the paths ${PATHS.join(', ')}`
const SHARED_TREE =
  'Shared live tree: never git stash|checkout|reset|restore|clean|commit, never make install|test, never edit settings.json. ' +
  (CWD ? 'Prefix every shell command with `cd ' + CWD + ' &&`. ' : '') + 'Comments 1 line ≤120 chars.'

const DEFAULT_DIMENSIONS = [
  { key: 'seams', prompt: 'dangling seams: callers of deleted or renamed symbols in comments, help text, registry rows, completion tables, retired-word lists; flags parsed but never read; fields declared but never set' },
  { key: 'regressions', prompt: 'behaviour regressions: a path that used to check or emit X and silently no longer does; a doc or dry-run surface pointing at a retired verb; a test rewritten to pin the broken output' },
  { key: 'tests', prompt: 'tests deleted alongside kept code, tests skipped in the range, guard tests that pin filenames the change moved, goldens that no longer match' },
]
const DIMENSIONS = Array.isArray(a.dimensions) && a.dimensions.length ? a.dimensions : DEFAULT_DIMENSIONS
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
const VERDICT_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['refuted', 'reason'],
  properties: { refuted: { type: 'boolean' }, reason: { type: 'string' } },
}
const RESULT_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  required: ['status', 'filesChanged', 'summary', 'leftUndone'],
  properties: {
    status: { type: 'string', enum: ['done', 'partial', 'blocked'] },
    filesChanged: { type: 'array', items: { type: 'string' } },
    summary: { type: 'string' },
    leftUndone: { type: 'array', items: { type: 'string' } },
  },
}

// ── Find → Refute as ONE pipeline: a group's findings are refuted while other groups still audit ──
const units = DIMENSIONS.flatMap(d => GROUPS.map((g, gi) => ({ dim: d, group: g, key: `${d.key}:${gi}` })))
log(`auditing ${TARGET} — ${DIMENSIONS.length} dimension(s) × ${GROUPS.length} package group(s)`)
const seen = new Set()
const refuted = []
const perUnit = await pipeline(
  units,
  u => agent(
    `Read-only audit of ${TARGET}${WHERE}. Packages: ${u.group.join(', ') || 'all touched'}. Lens: ${u.dim.prompt}.\n` +
    `Report only defects you can evidence with a grep, diff or test run — no style nits. ${SHARED_TREE}`,
    { label: `find:${u.key}`, phase: 'Find', schema: FINDINGS_SCHEMA, model: 'sonnet', effort: 'medium' }
  ),
  (found, u) => {
    const fresh = (found ? found.findings : []).filter(f => {
      const k = `${f.file}:${f.line}:${f.title}`
      if (seen.has(k)) return false
      seen.add(k)
      return true
    })
    return Promise.all(fresh.map(f =>
      agent(
        `Try to REFUTE this audit finding against the real tree${WHERE} (read-only). Default to refuted=true when the evidence does not hold up.\n` +
        `Finding: ${f.title}\nAt: ${f.file}:${f.line}\nScenario: ${f.scenario}\nEvidence: ${f.evidence}\n` +
        `Refuted means: the code path does not do what is claimed, the scenario cannot occur, or it is already handled. Quote the line that decides it.`,
        { label: `refute:${f.file.split('/').pop()}:${f.line}`, phase: 'Refute', schema: VERDICT_SCHEMA, model: 'sonnet', effort: 'medium' }
      ).then(v => {
        if (!v || v.refuted) { refuted.push({ ...f, reason: v ? v.reason : 'no verdict' }); return null }
        return { ...f, lens: u.dim.key }
      })
    ))
  }
)
const confirmed = perUnit.filter(Boolean).flat().filter(Boolean)
const order = { critical: 0, high: 1, medium: 2, low: 3 }
confirmed.sort((x, y) => order[x.severity] - order[y.severity])
log(`confirmed ${confirmed.length}, refuted ${refuted.length}`)
if (!FIX || !confirmed.length) return { confirmed, refuted, fixed: [], gate: null }

// ── Fix (ADK ParallelAgent per package: implementer ‖ test agent, disjoint file ownership) ────
phase('Fix')
const byPkg = new Map()
for (const f of confirmed) byPkg.set(f.package, [...(byPkg.get(f.package) || []), f])
const fixed = await parallel([...byPkg.entries()].map(([pkg, fs]) => () => {
  const list = fs.map(f => `- ${f.file}:${f.line} [${f.severity}] ${f.title}\n  scenario: ${f.scenario}`).join('\n')
  const ctx = `Confirmed defects in ${pkg}${WHERE}:\n${list}\n${SHARED_TREE}`
  return parallel([
    () => agent(`${ctx}\nYou are the IMPLEMENTER: fix every listed defect in non-test files of ${pkg} only; never *_test.go or testdata/. gofmt -l + go vet the package. Report per defect.`,
      { label: `fix:${pkg}`, phase: 'Fix', schema: RESULT_SCHEMA, model: 'opus', effort: 'high' }),
    () => agent(`${ctx}\nYou are the TEST agent: *_test.go and testdata/ in ${pkg} only. Repair tests the fixes break, add one regression test per defect pinning its scenario, run only ./${pkg}/... and report.`,
      { label: `test:${pkg}`, phase: 'Fix', schema: RESULT_SCHEMA, model: 'sonnet', effort: 'medium' }),
  ]).then(([impl, tests]) => ({ package: pkg, impl, tests }))
}))

// ── Gate (ADK LoopAgent via the saved gate-loop workflow) ────────────────────────────────────
phase('Gate')
const pkgs = [...byPkg.keys()]
const gate = await workflow('gate-loop', {
  gate: a.gate || `go build ./... && go vet ${pkgs.map(p => './' + p + '/').join(' ')} && go test ${pkgs.map(p => './' + p + '/').join(' ')}`,
  cwd: CWD || undefined, scope: pkgs, maxIterations: MAX_ITER,
})

return { confirmed, refuted, fixed: fixed.filter(Boolean), gate }
