/** @param {{ EXT: * }} input */
function engineerPreflightBrief({ EXT }) {
  return 'For each line, run it and report its candidate count and the first candidate as agent/account/model. Report what you find. First run `echo $HOME` and report it as home.\n' +
      EXT.map((s) => `claude-code gateway --agent ${s.agent} --fallback agents${s.profile ? ' --profile ' + s.profile : ''}${s.provider ? ' --provider ' + s.provider : ''} --format json </dev/null`).join('\n')
}

/** @param {{ CWD: * }} input */
function engineerRules({ CWD }) {
  return 'Shared live tree: never git stash|checkout|reset|restore|clean|commit|add, never make build|install|audit, never edit settings.json. ' +
  'Run every command in cwd ' + JSON.stringify(CWD) + ' (external agents: ask --workdir ' + CWD + ').'
}

/** @param {{ RANGE: *, RULES: * }} input */
function engineerRangeBrief({ RANGE, RULES }) {
  return RULES + ' Report only: run `git diff --name-only ' + RANGE + " -- '*.go'` and return the files that still exist and are not *_test.go, " +
      'repo-relative. Return {paths: []} when there are none.'
}

/** @param {{ PATHS: *, PKGS: *, RULES: *, TASK: * }} input */
function engineerSizeBrief({ PATHS, PKGS, RULES, TASK }) {
  return RULES + ' Report your findings. Do NOT pick a size; find facts. Read the code the task points at and return: kind (fix = repair ' +
      'existing behavior, feature = add behavior, refactor = change structure keeping existing behavior, audit = review packages for bugs, consolidate = merge duplicates and delete dead code, legacy paths or stale tests keeping behavior, architecture = a multi-session design change); the ' +
      'non-test source FILES that must change (repo-relative, one entry per file, never a directory, tests excluded); the Go package dirs ' +
      'only for an audit or consolidate; a one-line focus; and a one-line reason. Use `git status --short` and `go list ./...` only as needed.\nTask: ' + TASK +
      (PKGS.length ? '\nGiven packages: ' + PKGS.join(', ') : '') + (PATHS.length ? '\nGiven paths: ' + PATHS.join(', ') : '')
}

function engineerHomeProbe() {
  return 'Run `echo $HOME` and return it as home.'
}

/** @param {{ CWD: *, RULES: *, TASK: * }} input */
function engineerPlanBrief({ CWD, RULES, TASK }) {
  return RULES + ' Report your plan. Draft an implementation plan for this task in the shape of ' + CWD + '/docs/templates/plan.md (read it first), with the ' +
      'real files and phases found in the repo. Return the markdown in `plan`.\nTask: ' + TASK
}

/** @param {{ PATHS: *, RULES: *, TASK: * }} input */
function engineerIntelBrief({ PATHS, RULES, TASK }) {
  return RULES + ' Report your findings. Codebase intel for this task: structure, conventions, and the exact files to change with their callers. ' +
      'Return `paths` (repo-relative files to change or audit) and `focus` (the task plus the conventions and callers that matter).\nTask: ' + TASK +
      (PATHS.length ? '\nStart from: ' + PATHS.join(', ') : '')
}

/** @param {{ KIND: *, RULES: *, TASK: *, focus: *, implTargets: * }} input */
function engineerImplementationBrief({ KIND, RULES, TASK, focus, implTargets }) {
  return RULES + ' Make this change in these files (new files in the same package ok) and nothing else; smallest correct change, functions < 50 lines, 1-line comments. ' +
      'Run gofmt and go vet on the package. Do not write tests; ' + (KIND === 'feature' ? 'one Test stage follows for the feature, then ' : '') + 'code-workers follows with find, fix and gate. Return `files`: every repo-relative file you changed. ' +
      'Then run the changed surface live (the CLI verb, hook, endpoint or make target, e.g. `go run ./cmd/claude-code <verb> ...`) on real input and return `live`: {command, exit, output (trimmed tail)}; no runnable surface: {command: "", reason: "<why>"}.\nFiles: ' + implTargets.join(', ') + '\nTask: ' + TASK + '\nContext: ' + focus
}

/** @param {{ CWD: *, RULES: *, argv: *, note: * }} input */
function engineerPlanRunBrief({ CWD, RULES, argv, note }) {
  return RULES + ' Run exactly the argv below once from ' + CWD + ', shell-quoting every element and appending </dev/null; it is one of `claude-code agents ask --waves <plan> ...`, ' +
      '`claude-code agents wait <id> --timeout 10m` or `claude-code agents result <id> --verdict --lines 40`. Add no other flag or pipe.' + (note || '') +
      ' Return its exit code as `exit` (null when it could not run) and its stdout as `out` (the last 60 lines when longer).\nargv: ' + JSON.stringify(argv)
}

/** @param {{ RULES: *, need: *, w: * }} input */
function engineerApplySeamsBrief({ RULES, need, w }) {
  return RULES + ' Seam step after wave ' + w + ': the package fixers below left these changes undone because each changes an exported signature or a file outside its package. ' +
    'Apply them now and update every caller (grep the whole repo); smallest correct change; gofmt and go vet what you touched. Return `files`.\n' + need.map((x) => '- ' + x.pkg + ': ' + x.file + ' — ' + x.need).join('\n')
}

/** @param {{ LINE_BUDGET: *, RULES: *, ST_BUDGET: *, TASK: *, implOver: * }} input */
function engineerImplShrinkBrief({ LINE_BUDGET, RULES, ST_BUDGET, TASK, implOver }) {
  return RULES + ' Line budget ' + LINE_BUDGET + ' (production net ≤ ' + ST_BUDGET[LINE_BUDGET] + ' per package, git diff --numstat): your change measured ' +
    implOver().map(([p, n]) => p + ' ' + n).join(', ') + '. Shrink it or justify it: cut what the task does not need and keep the change; never revert it, never git checkout/restore/stash; cannot shrink: leave it, reason in summary. Return `files`.\nTask: ' + TASK
}

/** @param {{ IMPL_FILES: *, RULES: *, ST_PROPOSE: *, TASK: *, dirs: *, stDiff: * }} input */
function engineerImplTestBrief({ IMPL_FILES, RULES, ST_PROPOSE, TASK, dirs, stDiff }) {
  return RULES + ' A feature just landed (' + stDiff(dirs) + '; changed: ' + IMPL_FILES.join(', ') + '). Task: ' + TASK +
    '\nWrite basic tests for the feature\'s main behavior that call the real code (a couple of seconds at most), and repair any test the change broke. Edit only *_test.go and testdata files in ' + dirs.join(', ') + '.' + ST_PROPOSE + ' Put "n/a" in callers.'
}

/** @param {{ CWD: *, RULES: *, label: *, res: * }} input */
function engineerRecordOnceBrief({ CWD, RULES, label, res }) {
  return RULES + ' Record this run and nothing else. Write the JSON below verbatim to a NEW file from `mktemp "${TMPDIR:-/tmp}/record.XXXXXX"` ' +
    '(Read that empty file before you Write it), then run exactly `cd ' + CWD + ' && claude-code memory report --label ' + label + ' --file <that file> --format json </dev/null` ' +
    'and return its paths array. Change nothing else.\nJSON:\n' + JSON.stringify({ report: res.report, decisions: res.decisions })
}
