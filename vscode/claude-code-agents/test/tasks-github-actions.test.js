'use strict';
// Unit tests for the per-row detail/mutation surface of lib/tasks-github.js: pure argv builders,
// the async wrappers over an injectable execFileImpl, and the ghExec hygiene (env/timeout, retry,
// rate-limit breaker). Spec: AGENTS brief for vscode/claude-code-agents/lib/tasks-github.js.
const test = require('node:test');
const assert = require('node:assert/strict');
const tasksGithub = require('../lib/tasks-github');
const {
  PR_LIST_FIELDS,
  PR_DETAIL_FIELDS,
  mapPullRequest,
  mapIssue,
  prDetailArgs,
  issueDetailArgs,
  issueCloseArgs,
  issueReopenArgs,
  issueEditArgs,
  prReviewersArgs,
  prMergeArgs,
  autoMergeArgs,
  assignableArgs,
  mergeMethodsArgs,
  prDetail,
  issueDetail,
  closeIssue,
  reopenIssue,
  editIssue,
  editPrReviewers,
  mergePr,
  setAutoMerge,
  listAssignable,
  mergeMethods,
} = tasksGithub;

const REPO = { owner: 'o', name: 'r' };

test.beforeEach(() => {
  tasksGithub._resetBreaker();
  if (typeof tasksGithub.setRetryDelays === 'function') tasksGithub.setRetryDelays([0, 0]);
});
test.afterEach(() => {
  tasksGithub._resetBreaker();
  if (typeof tasksGithub.setRetryDelays === 'function') tasksGithub.setRetryDelays([250, 1000]);
});

// -- field sets --

test('PR_LIST_FIELDS is the light set with no eager hydration columns', () => {
  assert.equal(
    PR_LIST_FIELDS,
    'number,title,state,url,labels,updatedAt,author,isDraft,headRefName,baseRefName,headRefOid,headRepositoryOwner,reviewRequests,assignees'
  );
  for (const heavy of ['statusCheckRollup', 'mergeable', 'mergeStateStatus', 'reviewDecision', 'latestReviews', 'additions', 'deletions', 'changedFiles']) {
    assert.ok(!PR_LIST_FIELDS.includes(heavy), `PR_LIST_FIELDS must not include ${heavy}`);
  }
});

test('PR_DETAIL_FIELDS extends the light set with the hydration columns', () => {
  assert.ok(PR_DETAIL_FIELDS.startsWith(PR_LIST_FIELDS));
  for (const field of ['additions', 'deletions', 'changedFiles', 'reviewDecision', 'latestReviews', 'statusCheckRollup', 'mergeable', 'mergeStateStatus', 'autoMergeRequest']) {
    assert.ok(PR_DETAIL_FIELDS.includes(field), `PR_DETAIL_FIELDS must include ${field}`);
  }
});

// -- mapPullRequest hydration defaults --

test('mapPullRequest: an open PR with no statusCheckRollup is unhydrated with loading checks/merge', () => {
  const row = mapPullRequest({ number: 1, title: 't', state: 'OPEN', url: 'u', updatedAt: '2026-01-01T00:00:00Z' }, REPO);
  assert.equal(row.hydrated, false);
  assert.deepEqual(row.checks, { state: 'loading', label: '…', counts: { success: 0, failed: 0, pending: 0, skipped: 0 } });
  assert.deepEqual(row.merge, { state: 'loading', label: '…' });
});

test('mapPullRequest: a draft PR with no statusCheckRollup is unhydrated too', () => {
  const row = mapPullRequest({ number: 2, title: 't', state: 'OPEN', isDraft: true, url: 'u', updatedAt: '2026-01-01T00:00:00Z' }, REPO);
  assert.equal(row.state, 'draft');
  assert.equal(row.hydrated, false);
  assert.equal(row.merge.state, 'loading');
});

test('mapPullRequest: merged/closed PRs stay hydrated:true with their terminal merge state even without statusCheckRollup', () => {
  const merged = mapPullRequest({ number: 3, title: 't', state: 'MERGED', mergedAt: '2026-01-02T00:00:00Z', url: 'u', updatedAt: '2026-01-02T00:00:00Z' }, REPO);
  assert.equal(merged.hydrated, true);
  assert.deepEqual(merged.merge, { state: 'merged', label: 'Merged' });
  const closed = mapPullRequest({ number: 4, title: 't', state: 'CLOSED', url: 'u', updatedAt: '2026-01-02T00:00:00Z' }, REPO);
  assert.equal(closed.hydrated, true);
  assert.deepEqual(closed.merge, { state: 'closed', label: 'Closed' });
});

test('mapPullRequest: statusCheckRollup present (even empty array) marks the row hydrated', () => {
  const row = mapPullRequest({ number: 5, title: 't', state: 'OPEN', url: 'u', updatedAt: '2026-01-01T00:00:00Z', statusCheckRollup: [], mergeable: 'MERGEABLE', mergeStateStatus: 'CLEAN' }, REPO);
  assert.equal(row.hydrated, true);
  assert.equal(row.checks.state, 'none');
  assert.deepEqual(row.merge, { state: 'ready', label: 'Ready' });
});

test('mapPullRequest: autoMergeEnabled reflects autoMergeRequest presence', () => {
  assert.equal(mapPullRequest({ number: 6, title: 't', state: 'OPEN', url: 'u', updatedAt: '2026-01-01T00:00:00Z' }, REPO).autoMergeEnabled, false);
  assert.equal(mapPullRequest({ number: 7, title: 't', state: 'OPEN', url: 'u', updatedAt: '2026-01-01T00:00:00Z', autoMergeRequest: { mergeMethod: 'SQUASH' } }, REPO).autoMergeEnabled, true);
});

// -- mapIssue: gh issue view --json camelCase shape --

test('mapIssue: accepts the gh issue view camelCase shape (state, stateReason, url, author.login, updatedAt, assignees[].login)', () => {
  const row = mapIssue({
    number: 7,
    title: 'Fix the thing',
    state: 'OPEN',
    stateReason: null,
    url: 'https://github.com/o/r/issues/7',
    updatedAt: '2026-02-01T00:00:00Z',
    author: { login: 'dev1' },
    assignees: [{ login: 'dev2' }, { login: 'dev3' }],
    labels: [],
  }, REPO);
  assert.equal(row.state, 'open');
  assert.equal(row.stateReason, null);
  assert.equal(row.url, 'https://github.com/o/r/issues/7');
  assert.equal(row.author, 'dev1');
  assert.equal(row.updatedAt, '2026-02-01T00:00:00Z');
  assert.deepEqual(row.assignees, ['dev2', 'dev3']);
});

test('mapIssue: CLOSED camelCase state folds to closed', () => {
  const row = mapIssue({ number: 8, title: 't', state: 'CLOSED', url: 'u', updatedAt: 'x', author: { login: 'a' }, assignees: [] }, REPO);
  assert.equal(row.state, 'closed');
});

// -- pure argv builders --

test('prDetailArgs builds gh pr view --json PR_DETAIL_FIELDS', () => {
  assert.deepEqual(prDetailArgs({ repo: REPO, number: 42 }), ['pr', 'view', '42', '--repo', 'o/r', '--json', PR_DETAIL_FIELDS]);
});

test('issueDetailArgs builds gh issue view --json with the issue detail fields', () => {
  assert.deepEqual(issueDetailArgs({ repo: REPO, number: 7 }), ['issue', 'view', '7', '--repo', 'o/r', '--json', 'number,title,state,stateReason,url,labels,updatedAt,author,assignees']);
});

test('issueCloseArgs: reason completed / not planned, duplicate-of wins over reason', () => {
  assert.deepEqual(issueCloseArgs({ repo: REPO, number: 7, reason: 'completed' }), ['issue', 'close', '7', '--repo', 'o/r', '--reason', 'completed']);
  assert.deepEqual(issueCloseArgs({ repo: REPO, number: 7, reason: 'not_planned' }), ['issue', 'close', '7', '--repo', 'o/r', '--reason', 'not planned']);
  assert.deepEqual(issueCloseArgs({ repo: REPO, number: 7, reason: 'completed', duplicateOf: 9 }), ['issue', 'close', '7', '--repo', 'o/r', '--duplicate-of', '9']);
  assert.deepEqual(issueCloseArgs({ repo: REPO, number: 7 }), ['issue', 'close', '7', '--repo', 'o/r']);
});

test('issueReopenArgs builds gh issue reopen', () => {
  assert.deepEqual(issueReopenArgs({ repo: REPO, number: 7 }), ['issue', 'reopen', '7', '--repo', 'o/r']);
});

test('issueEditArgs: add/remove assignee flags, omitted for empty lists', () => {
  assert.deepEqual(
    issueEditArgs({ repo: REPO, number: 7, addAssignees: ['a', 'b'], removeAssignees: ['c'] }),
    ['issue', 'edit', '7', '--repo', 'o/r', '--add-assignee', 'a,b', '--remove-assignee', 'c']
  );
  assert.deepEqual(issueEditArgs({ repo: REPO, number: 7, addAssignees: [], removeAssignees: [] }), ['issue', 'edit', '7', '--repo', 'o/r']);
});

test('prReviewersArgs: add/remove reviewer flags', () => {
  assert.deepEqual(prReviewersArgs({ repo: REPO, number: 42, add: ['x'], remove: ['y'] }), ['pr', 'edit', '42', '--repo', 'o/r', '--add-reviewer', 'x', '--remove-reviewer', 'y']);
});

test('prMergeArgs: squash/merge/rebase flags, invalid method throws', () => {
  assert.deepEqual(prMergeArgs({ repo: REPO, number: 42, method: 'squash' }), ['pr', 'merge', '42', '--repo', 'o/r', '--squash']);
  assert.deepEqual(prMergeArgs({ repo: REPO, number: 42, method: 'merge' }), ['pr', 'merge', '42', '--repo', 'o/r', '--merge']);
  assert.deepEqual(prMergeArgs({ repo: REPO, number: 42, method: 'rebase' }), ['pr', 'merge', '42', '--repo', 'o/r', '--rebase']);
  assert.throws(() => prMergeArgs({ repo: REPO, number: 42, method: 'bogus' }));
});

test('autoMergeArgs: enable with method, disable ignores method', () => {
  assert.deepEqual(autoMergeArgs({ repo: REPO, number: 42, method: 'squash', enable: true }), ['pr', 'merge', '42', '--repo', 'o/r', '--auto', '--squash']);
  assert.deepEqual(autoMergeArgs({ repo: REPO, number: 42, method: 'squash', enable: false }), ['pr', 'merge', '42', '--repo', 'o/r', '--disable-auto']);
});

test('assignableArgs builds the paginated repo assignees api call', () => {
  assert.deepEqual(assignableArgs({ repo: REPO }), ['api', '--paginate', 'repos/o/r/assignees?per_page=100', '--jq', '.[].login']);
});

test('mergeMethodsArgs builds the repo api call with the merge/squash/rebase jq projection', () => {
  assert.deepEqual(mergeMethodsArgs({ repo: REPO }), ['api', 'repos/o/r', '--jq', '{merge: .allow_merge_commit, squash: .allow_squash_merge, rebase: .allow_rebase_merge}']);
});

// -- async wrappers over an injectable execFileImpl --

function fakeExecOnce(stdout) {
  return (cmd, args, opts, cb) => cb(null, stdout, '');
}

test('prDetail resolves to mapPullRequest(JSON.parse(stdout))', async () => {
  const item = { number: 42, title: 'PR', state: 'OPEN', url: 'u', updatedAt: 'x', statusCheckRollup: [] };
  const row = await prDetail({ repo: REPO, number: 42, execFileImpl: fakeExecOnce(JSON.stringify(item)) });
  assert.equal(row.id, 'github:o/r:pr:42');
  assert.equal(row.hydrated, true);
});

test('issueDetail resolves to mapIssue(JSON.parse(stdout))', async () => {
  const item = { number: 7, title: 'Issue', state: 'OPEN', url: 'u', updatedAt: 'x', author: { login: 'a' }, assignees: [] };
  const row = await issueDetail({ repo: REPO, number: 7, execFileImpl: fakeExecOnce(JSON.stringify(item)) });
  assert.equal(row.id, 'github:o/r:issue:7');
  assert.equal(row.state, 'open');
});

test('closeIssue, reopenIssue, editIssue, editPrReviewers, mergePr, setAutoMerge all resolve {ok:true}', async () => {
  const exec = fakeExecOnce('');
  assert.deepEqual(await closeIssue({ repo: REPO, number: 7, execFileImpl: exec }), { ok: true });
  assert.deepEqual(await reopenIssue({ repo: REPO, number: 7, execFileImpl: exec }), { ok: true });
  assert.deepEqual(await editIssue({ repo: REPO, number: 7, addAssignees: ['a'], execFileImpl: exec }), { ok: true });
  assert.deepEqual(await editPrReviewers({ repo: REPO, number: 42, add: ['x'], execFileImpl: exec }), { ok: true });
  assert.deepEqual(await mergePr({ repo: REPO, number: 42, method: 'squash', execFileImpl: exec }), { ok: true });
  assert.deepEqual(await setAutoMerge({ repo: REPO, number: 42, method: 'squash', enable: true, execFileImpl: exec }), { ok: true });
});

test('listAssignable splits stdout lines into logins', async () => {
  const logins = await listAssignable({ repo: REPO, execFileImpl: fakeExecOnce('dev1\ndev2\n\ndev3\n') });
  assert.deepEqual(logins, ['dev1', 'dev2', 'dev3']);
});

test('mergeMethods parses the merge/squash/rebase booleans', async () => {
  const methods = await mergeMethods({ repo: REPO, execFileImpl: fakeExecOnce('{"merge": true, "squash": false, "rebase": true}') });
  assert.deepEqual(methods, { merge: true, squash: false, rebase: true });
});

// -- ghExec hygiene: env/timeout, retry, breaker --

test('ghExec passes GH_PROMPT_DISABLED=1 and a 30000ms timeout to execFileImpl', async () => {
  let seenOpts;
  const execFileImpl = (cmd, args, opts, cb) => {
    seenOpts = opts;
    cb(null, 'ok', '');
  };
  await listAssignable({ repo: REPO, execFileImpl });
  assert.equal(seenOpts.env.GH_PROMPT_DISABLED, '1');
  assert.equal(seenOpts.timeout, 30000);
});

test('ghExec retries a transient HTTP 502 up to 3 attempts total, then rejects', async () => {
  let calls = 0;
  const execFileImpl = (cmd, args, opts, cb) => {
    calls++;
    const err = new Error('fail');
    cb(err, '', 'HTTP 502 Bad Gateway');
  };
  await assert.rejects(() => listAssignable({ repo: REPO, execFileImpl }));
  assert.equal(calls, 3);
});

test('ghExec: a transient failure that succeeds on attempt 2 resolves', async () => {
  let calls = 0;
  const execFileImpl = (cmd, args, opts, cb) => {
    calls++;
    if (calls === 1) return cb(new Error('fail'), '', 'HTTP 503 Service Unavailable');
    cb(null, 'dev1\n', '');
  };
  const logins = await listAssignable({ repo: REPO, execFileImpl });
  assert.deepEqual(logins, ['dev1']);
  assert.equal(calls, 2);
});

test('ghExec: HTTP 429 without Retry-After is transient; HTTP 429 with Retry-After is not', async () => {
  let calls = 0;
  const noRetryAfter = (cmd, args, opts, cb) => { calls++; cb(new Error('fail'), '', 'HTTP 429 Too Many Requests'); };
  await assert.rejects(() => listAssignable({ repo: REPO, execFileImpl: noRetryAfter }));
  assert.equal(calls, 3, 'HTTP 429 with no Retry-After retries like other transient errors');

  tasksGithub._resetBreaker();
  let calls2 = 0;
  const withRetryAfter = (cmd, args, opts, cb) => { calls2++; cb(new Error('fail'), '', 'HTTP 429 Too Many Requests\nRetry-After: 30'); };
  await assert.rejects(() => listAssignable({ repo: REPO, execFileImpl: withRetryAfter }));
  assert.equal(calls2, 1, 'HTTP 429 with a Retry-After header is not retried');
});

test('ghExec: ECONNRESET and "socket hang up" are transient', async () => {
  let calls = 0;
  const econnreset = (cmd, args, opts, cb) => {
    calls++;
    const err = new Error('reset');
    err.code = 'ECONNRESET';
    cb(err, '', '');
  };
  await assert.rejects(() => listAssignable({ repo: REPO, execFileImpl: econnreset }));
  assert.equal(calls, 3);

  tasksGithub._resetBreaker();
  let calls2 = 0;
  const hangup = (cmd, args, opts, cb) => { calls2++; cb(new Error('socket hang up'), '', ''); };
  await assert.rejects(() => listAssignable({ repo: REPO, execFileImpl: hangup }));
  assert.equal(calls2, 3);
});

test('ghExec: a non-transient HTTP 404 is not retried (1 call)', async () => {
  let calls = 0;
  const execFileImpl = (cmd, args, opts, cb) => {
    calls++;
    cb(new Error('fail'), '', 'HTTP 404 Not Found');
  };
  await assert.rejects(() => listAssignable({ repo: REPO, execFileImpl }));
  assert.equal(calls, 1);
});

test('rate-limit breaker: a primary "API rate limit exceeded" blocks the next call in the same bucket', async () => {
  let calls = 0;
  const limited = (cmd, args, opts, cb) => { calls++; cb(new Error('fail'), '', 'API rate limit exceeded for user'); };
  await assert.rejects(() => listAssignable({ repo: REPO, execFileImpl: limited }));
  assert.equal(calls, 1, 'the rate-limit error itself is not retried');

  let secondCalled = false;
  await assert.rejects(
    () => listAssignable({ repo: REPO, execFileImpl: () => { secondCalled = true; } }),
    /rate limit.*retry/i
  );
  assert.equal(secondCalled, false, 'the next call in the blocked bucket never reaches execFileImpl');
});

test('rate-limit breaker: "secondary rate limit" text does NOT trip the breaker', async () => {
  let calls = 0;
  const secondary = (cmd, args, opts, cb) => { calls++; cb(new Error('fail'), '', 'you have exceeded a secondary rate limit'); };
  await assert.rejects(() => listAssignable({ repo: REPO, execFileImpl: secondary }));
  assert.equal(calls, 1, 'secondary rate limit text is not transient either, so no retry');

  let secondCalled = false;
  const okNext = (cmd, args, opts, cb) => { secondCalled = true; cb(null, 'dev1\n', ''); };
  await listAssignable({ repo: REPO, execFileImpl: okNext });
  assert.equal(secondCalled, true, 'the breaker was not tripped, so the next call still reaches execFileImpl');
});

test('rate-limit breaker: only the triggering bucket (core) is blocked; search is unaffected', async () => {
  const limited = (cmd, args, opts, cb) => cb(new Error('fail'), '', 'API rate limit exceeded for user');
  await assert.rejects(() => listAssignable({ repo: REPO, execFileImpl: limited })); // core bucket (gh api repos/...)

  let searchCalled = false;
  const okSearch = (cmd, args, opts, cb) => { searchCalled = true; cb(null, '{"items": []}', ''); };
  const { listWorkItems } = tasksGithub;
  await listWorkItems({ repos: [REPO], query: { scope: 'issue', state: null, labels: [] }, execFileImpl: okSearch });
  assert.equal(searchCalled, true, 'the search bucket is independent of the blocked core bucket');
});

test('_resetBreaker clears a tripped breaker', async () => {
  const limited = (cmd, args, opts, cb) => cb(new Error('fail'), '', 'API rate limit exceeded for user');
  await assert.rejects(() => listAssignable({ repo: REPO, execFileImpl: limited }));
  tasksGithub._resetBreaker();
  const logins = await listAssignable({ repo: REPO, execFileImpl: fakeExecOnce('dev1\n') });
  assert.deepEqual(logins, ['dev1']);
});
