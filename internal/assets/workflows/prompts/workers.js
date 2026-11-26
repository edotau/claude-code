/** @param {{ JOB: * }} input */
function workersLenses({ JOB }) {
  return JOB === 'consolidate' ? [
  { key: 'duplicate', ask: 'duplicated helpers or logic (inside the package or against a sibling), legacy paths and shims no caller needs' },
  { key: 'dead', ask: 'dead code with no caller (verify with grep), stale or redundant tests that pin nothing the other tests miss' },
] : [
  { key: 'correctness', ask: 'wrong results, bad parsing, state that disagrees with itself, broken invariants' },
  { key: 'failure', ask: 'swallowed or mishandled errors, partial writes, records or locks left behind, races, leaked secrets' },
]
}

/** @param {{ a: * }} input */
function workersFocus({ a }) {
  return a.focus ? ' Focus for this group: ' + a.focus + '.' : ''
}

/** @param {{ CWD: * }} input */
function workersRules({ CWD }) {
  return 'Shared live tree, other packages are being edited in parallel: never git stash|checkout|reset|restore|clean|commit|add, never ' +
  'make build|install|audit, never edit settings.json. Run every command in cwd ' + JSON.stringify(CWD) + ' (external agents: ask --workdir ' + CWD + ').'
}

/** @param {{ RULES: *, a: * }} input */
function workersRo({ RULES, a }, hint) {
  return RULES + ' Report your findings; never run go test.' + (/test/i.test((a.focus || '') + ' ' + hint) ? ' *_test.go files are in scope.' : ' Skip *_test.go.')
}

const WORKERS_JUDGMENTS = {
  merge: 'It is a consolidation candidate, not a bug. Cite the twin it duplicates or the empty caller grep; check that merging or deleting it keeps behaviour, messages and exit codes identical. ' +
    'reproduced=true only when both hold; put the citation and why the merge is safe in scenario, and rate its severity.',
  cost: 'It has no failing scenario by nature. Check the cost is real: cite the spawn, loop, sleep or complexity line (measure it if cheap). ' +
    'Check the fix keeps behavior. reproduced=true only when the cost is real and the change is safe; put the cited line and why the fix is safe in scenario, and rate its severity.',
  bug: 'Derive your own failing scenario from the code and rate its severity; reproduced=false if you cannot.',
}

/** @param {{ MERGE_KINDS: *, stCost: * }} input */
function workersJudge({ MERGE_KINDS, stCost }, f) {
  if (MERGE_KINDS.includes(f.kind)) return WORKERS_JUDGMENTS.merge
  return stCost(f) ? WORKERS_JUDGMENTS.cost : WORKERS_JUDGMENTS.bug
}

/** @param {{ FOCUS: *, JOB: * }} input */
function workersFindAsk({ FOCUS, JOB }, l) {
  return JOB === 'consolidate'
  ? ' Find consolidation candidates (not bugs) of this kind: ' + l.ask + '.' + FOCUS + ' Each finding: file, line, symbol, a one-line symptom, kind duplicate|dead|legacy|stale-test, ' +
    'and as its scenario the twin it duplicates or the empty caller grep. Return {findings: []} when clean.'
  : ' Find real bugs of this kind: ' + l.ask + '.' + FOCUS + ' Each finding: file, line, function or symbol, a one-line symptom, a concrete failing scenario, kind bug. A perf, runtime, complexity or ' +
    'test-quality cost with no failing scenario takes that kind and names the cost (spawn, loop, sleep, complexity) as its scenario. Return {findings: []} when clean.'
}

/** @param {{ FILES: *, FOCUS: *, JOB: *, RULES: *, a: *, l: *, p: * }} input */
function workersFindFrame({ FILES, FOCUS, JOB, RULES, a, l, p }) {
  return workersRo({ RULES, a }, l.ask) + ' Package ' + p + ' (its own directory and nested packages).' + (FILES[p] ? ' Focus on these files: ' + FILES[p].join(', ') + '.' : '') + workersFindAsk({ FOCUS, JOB }, l)
}

/** @param {{ MERGE_KINDS: *, RULES: *, a: *, f: *, stCost: * }} input */
function workersRederiveFrame({ MERGE_KINDS, RULES, a, f, stCost }) {
  return workersRo({ RULES, a }, f.file + ' ' + (f.kind || '')) + ' A teammate says there is ' + (stCost(f) ? 'a ' + f.kind + ' cost' : 'a bug') + ' at ' + f.file + ':' + f.line + ' (' + f.symbol + ') with this symptom: ' + f.symptom +
        '. You have not seen their reasoning. ' + workersJudge({ MERGE_KINDS, stCost }, f)
}

/** @param {{ MERGE_KINDS: *, RULES: *, a: *, stCost: *, x: * }} input */
function workersSeverityFrame({ MERGE_KINDS, RULES, a, stCost, x }) {
  return workersRo({ RULES, a }, x.f.file + ' ' + (x.f.kind || '')) + ' Rate the severity of a possible ' + (stCost(x.f) ? x.f.kind + ' cost' : 'bug') + ' at ' + x.f.file + ':' + x.f.line + ' (' + x.f.symbol + '): ' + x.f.symptom +
      '. ' + workersJudge({ MERGE_KINDS, stCost }, x.f)
}
