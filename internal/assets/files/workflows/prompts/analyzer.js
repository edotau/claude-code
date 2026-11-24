function analyzerDefaultDimensions() {
  return [
  { key: 'seams', prompt: 'dangling seams: callers of deleted or renamed symbols in comments, help text, docs, registration/config tables; flags parsed but never read; fields declared but never set' },
  { key: 'regressions', prompt: 'behaviour regressions: a path that used to check or emit X and silently no longer does; a doc, help or dry-run surface pointing at a removed symbol; a test rewritten to pin the broken output' },
  { key: 'tests', prompt: 'tests deleted alongside kept code, tests skipped in the range, guard tests that pin filenames the change moved, goldens that no longer match', effort: 'low' },
]
}

function analyzerCodeDimensions() {
  return [
  { key: 'correctness', prompt: 'logic defects: wrong conditionals, off-by-one, nil/empty/zero-value paths that produce a wrong result or a crash, error values checked then ignored' },
  { key: 'failure-modes', prompt: 'error handling: swallowed errors, fail-open where the comment or name promises fail-closed, exec/HTTP without a timeout or context, locks that can be skipped, partial writes' },
  { key: 'contracts', prompt: 'contract drift: help text, comments or docs that disagree with the code; flags parsed but never read; fields declared but never set; exported symbols with no caller (verify with grep)', effort: 'low', hitsOnly: true },
]
}

function analyzerConsolidateDimensions() {
  return [
  { key: 'duplicates', prompt: 'consolidation candidates, not bugs: duplicated helpers or logic across files or packages, legacy paths and shims no caller needs (verify with grep)' },
  { key: 'dead', prompt: 'consolidation candidates, not bugs: dead code with no caller (verify with grep), stale or redundant tests that pin nothing the other tests miss', effort: 'low' },
]
}

/** @param {{ CWD: * }} input */
function analyzerSharedTree({ CWD }) {
  return 'Shared live tree: never git stash|checkout|reset|restore|clean|commit, never make build|install|audit, never edit settings.json. ' +
  (CWD ? 'cwd: ' + CWD + ' (external agents: ask --workdir ' + CWD + '). Prefix every shell command with `cd ' + CWD + ' &&` (each Bash call starts in the session dir; the copilot shell allows a cd inside its roots). ' : '') + 'Comments 1 line ≤120 chars.'
}

/** @param {{ EXT: * }} input */
function analyzerPreflightBrief({ EXT }) {
  return 'For each line, run it and report its candidate count and the first candidate as agent/account/model. Report what you find. Also run `claude-code gateway --format json --agent claude --fallback none --model sonnet </dev/null` and report claudeState = candidates[0].state when candidates is non-empty, else the skipped[] entry for claude and its gate value, else "unknown".\n' +
      EXT.map((s) => `claude-code gateway --agent ${s.agent} --fallback agents${s.profile ? ' --profile ' + s.profile : ''}${s.provider ? ' --provider ' + s.provider : ''} --format json </dev/null`).join('\n')
}

/** @param {{ CD: * }} input */
function analyzerRouteStatePBrief({ CD }) {
  return `Run exactly: ${CD}claude-code gateway --format json --agent claude --fallback none --model sonnet. ` +
    `It prints one JSON object; state = candidates[0].state when candidates is non-empty, else the skipped[] entry whose agent ` +
    `is "claude" and its gate value, else "unknown". Never run anything else.`
}

/** @param {{ a: * }} input */
function analyzerFocus({ a }) {
  return a.focus ? ` Caller focus: ${a.focus}.` : ''
}

/** @param {{ CD: *, CWD: *, PATHS: * }} input */
function analyzerSplitPBrief({ CD, CWD, PATHS }) {
  return `Run exactly: ${CD}claude-code review scope --repo ${CWD || '.'} --paths ${PATHS.join(',')} --format json </dev/null | ` +
      `jq -c '[.files[].path | sub("/[^/]*$"; "")] | group_by(.) | map({dir: .[0], files: length})' — and return its array verbatim as dirs. Never run anything else.`
}

/** @param {{ CWD: *, FILES_MODE: *, PATHS: *, RANGE: *, SPLIT: * }} input */
function analyzerScopeCmd({ CWD, FILES_MODE, PATHS, RANGE, SPLIT }, g) {
  return SPLIT
  ? `claude-code review scope --repo ${CWD || '.'} --paths ${PATHS.join(',')} --format json </dev/null | jq -c --argjson d '${JSON.stringify(g)}' '{files: [.files[] | select(.path | sub("/[^/]*$"; "") | IN($d[]))]}'`
  : `claude-code review scope --repo ${CWD || '.'} ${FILES_MODE ? '' : '--range ' + (RANGE || 'HEAD~1..HEAD') + ' '}--paths ${g.join(',') || '.'} --format json </dev/null`
}

/** @param {{ CD: *, CWD: *, FILES_MODE: *, PATHS: *, RANGE: *, SPLIT: *, g: * }} input */
function analyzerRunScopesBrief({ CD, CWD, FILES_MODE, PATHS, RANGE, SPLIT, g }) {
  return `${CD}run ONE command: ${analyzerScopeCmd({ CWD, FILES_MODE, PATHS, RANGE, SPLIT }, g)} — and return its files array verbatim ({path, bytes}). Do not read the files; the auditors cat their own chunk. At most 2 tool calls.`
}

/** @param {{ CWD: * }} input */
function analyzerWorkingDirectory({ CWD }) {
  return CWD ? `cwd: ${CWD} (external agents: ask --workdir ${CWD}). Prefix every shell command with \`cd ${CWD} &&\` (each Bash call starts in the session dir; the copilot shell allows a cd inside its roots).\n` : ''
}

function analyzerGateRules() {
  return 'Never run git stash|checkout|reset|restore|clean|commit; never run install/deploy/release targets or edit tool config. Edit only inside scope; re-run only the gate.'
}

/** @param {{ CD: *, SHARED_TREE: *, dryRun: *, json: *, label: * }} input */
function analyzerRecordReportBrief({ CD, SHARED_TREE, dryRun, json, label }) {
  return SHARED_TREE + ' Record this and nothing else. First run `date +%H%M%S` as its own command. Write the JSON below verbatim to a NEW file from `mktemp "${TMPDIR:-/tmp}/record.XXXXXX"` ' +
    '(Read that empty file before you Write it), then run exactly `' + CD + 'claude-code memory report --label ' + label + '-<that time> --file <that file> --format json </dev/null` ' +
    'and return its paths array (the first path is the report file).' + (dryRun ? ' Then run exactly `' + CD + 'claude-code agents ask --waves <that first path> --dry-run </dev/null` and return its exit code as dryRunExit.' : '') + json
}

/** @param {{ CD: *, FILES_MODE: *, RANGE: * }} input */
function analyzerFindFirstCall({ CD, FILES_MODE, RANGE }) {
  return FILES_MODE
  ? `First tool call: cat the files listed below (one call, all of them). Judge from that; open another file only to check a caller or a test (budget 8 tool calls). `
  : `First tool call: ${CD}git diff ${RANGE || 'HEAD~1..HEAD'} -- <the files listed below> (one call, all of them). Judge from that diff; open a file only to check a caller or a test named in it (budget 8 tool calls). `
}

/** @param {{ FILES_MODE: *, RANGE: * }} input */
function analyzerFindFiles({ FILES_MODE, RANGE }, u) {
  return u.chunk.files.length
  ? `Files: ${u.chunk.files.map(f => f.path).join(' ')}\n\n`
  : `(the scope agent returned nothing — ${FILES_MODE ? 'read the files under ' + u.group.join(' ') : 'run git diff --stat ' + (RANGE || 'HEAD~1..HEAD') + ' -- ' + (u.group.join(' ') || '.')} yourself)`
}

/** @param {{ CD: *, FILES_MODE: *, FOCUS: *, IN: *, RANGE: *, SHARED_TREE: *, TARGET: * }} input */
function analyzerFindPrompt({ CD, FILES_MODE, FOCUS, IN, RANGE, SHARED_TREE, TARGET }, u) {
  return `Audit of ${TARGET}${IN}. Packages: ${u.group.join(', ') || 'all touched'}. Lens: ${u.dim.prompt}.${FOCUS}\n` + analyzerFindFirstCall({ CD, FILES_MODE, RANGE }) +
  `Report only defects you can evidence with a grep, diff or test run — no style nits. ${SHARED_TREE}\n` +
  `Method: ~/.claude/skills/evaluate-code/SKILL.md (report only findings on changed code you verified; >80% confidence filter)\n\n` + analyzerFindFiles({ FILES_MODE, RANGE }, u)
}

/** @param {{ FILES_MODE: *, RANGE: * }} input */
function analyzerRefuteAt({ FILES_MODE, RANGE }) {
  const end = (RANGE || 'HEAD~1..HEAD').split(/\.{2,3}/).pop() || 'HEAD'
  return FILES_MODE ? '' : `These findings describe the change ${RANGE || 'HEAD~1..HEAD'}: judge the code as of its end commit ${end} (git show ${end}:<path>, git grep <pattern> ${end}) wherever the working tree differs from it; later commits do not refute a finding. `
}

function analyzerRefuteRules() {
  return `Refuted means: the code path does not do what is claimed, the scenario cannot occur, or it is already handled; quote the line that decides it. ` +
  `Default to refuted=true when the evidence does not hold up. A TRUE claim with a WRONG file or line is NOT refuted: set refuted=false and give the correct file and line. ` +
  `That a change was deliberate, already merged or allowed by a repo rule does NOT refute a finding: only facts about the code do. ` +
  `A claim that a verb, flag, symbol or file does NOT exist is refuted unless a tree-wide grep (alias, registration and config tables included) AND the repo's own test or CLI output both come back empty.`
}

/** @param {{ FILES_MODE: *, IN: *, RANGE: *, REFUTE_RULES: * }} input */
function analyzerRefuteBrief({ FILES_MODE, IN, RANGE, REFUTE_RULES }, pkg, fs) {
  const list = fs.map((f, i) => `${i}. ${f.title}\n   At: ${f.file}:${f.line}\n   Scenario: ${f.scenario}\n   Evidence: ${f.evidence}`).join('\n')
  return `Try to REFUTE each of these ${fs.length} audit finding(s) in ${pkg} against the real tree${IN} ; one verdict per index.\n${list}\n\n` + analyzerRefuteAt({ FILES_MODE, RANGE }) + REFUTE_RULES
}

/** @param {{ CD: *, CWD: *, HOT: *, IN: *, SHARED_TREE: *, fs: *, pkg: *, relPath: * }} input */
function analyzerDependencyScanBrief({ CD, CWD, HOT, IN, SHARED_TREE, fs, pkg, relPath }) {
  return `Dependency scan of ${pkg}${IN}. A fixer will repair these findings:\n${fs.map(f => `- ${relPath(f.file)}:${f.line} ${f.title}`).join('\n')}\n` +
      `Return files = every file the fix will likely edit (tests included), exported = whether it changes an exported symbol or signature, ` +
      `importers = repo packages importing ${pkg} (run once: ${CD}claude-code review scope --repo ${CWD || '.'} --paths ${pkg} --importers --format json </dev/null and take packages[].importers), ` +
      `hotspots = those of ${HOT.join(', ')} the fix must also edit. At most 5 tool calls. ${SHARED_TREE}`
}
