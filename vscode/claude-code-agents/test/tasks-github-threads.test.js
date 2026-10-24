'use strict';
// Unit tests for the review-thread / PR-mutation additions to lib/tasks-github.js: argv builders
// (reviewThreadsArgs, resolveThreadArgs, replyArgs, prTitleArgs, rerunFailedArgs), the mapper
// (mapReviewThreads), and the execFileImpl wrappers (reviewThreads, setThreadResolved,
// replyToThread, setPrTitle, rerunFailedChecks). Spec: this session's AGENTS brief for
// vscode/claude-code-agents/lib/tasks-github.js (review threads + PR title/rerun).
const test = require('node:test');
const assert = require('node:assert/strict');
const {
  reviewThreadsArgs,
  mapReviewThreads,
  resolveThreadArgs,
  replyArgs,
  prTitleArgs,
  rerunFailedArgs,
  mapCheckRuns,
  reviewThreads,
  setThreadResolved,
  replyToThread,
  setPrTitle,
  rerunFailedChecks,
} = require('../lib/tasks-github');

const REPO = { owner: 'o', name: 'r' };
const NUMBER = 7;

// -- reviewThreadsArgs --

test('reviewThreadsArgs: gh api graphql with owner/repo/pr -F variables, query requests reviewThreads/bodyHTML', () => {
  const args = reviewThreadsArgs({ repo: REPO, number: NUMBER });
  assert.equal(args[0], 'api');
  assert.equal(args[1], 'graphql');
  assert.ok(args.includes('graphql'));
  const fIdx = args.indexOf('-f');
  assert.ok(fIdx >= 0);
  const query = args[fIdx + 1];
  assert.match(query, /^query=/);
  assert.match(query, /reviewThreads/);
  assert.match(query, /bodyHTML/);
  const ownerIdx = args.indexOf('owner=o');
  assert.ok(args.some((a) => a === 'owner=o'));
  assert.ok(args.some((a) => a === 'repo=r'));
  const prFlagIdx = args.indexOf('-F');
  assert.ok(prFlagIdx >= 0, 'pr variable must be passed with -F (typed, not -f)');
  assert.equal(args[prFlagIdx + 1], 'pr=7');
});

test('reviewThreadsArgs: query text shapes the GraphQL selection set (owner/name/pr variables, thread + comment fields)', () => {
  const args = reviewThreadsArgs({ repo: REPO, number: NUMBER });
  const query = args[args.indexOf('-f') + 1];
  for (const needle of ['repository(owner:$owner,name:$repo)', 'pullRequest(number:$pr)', 'isResolved', 'isOutdated', 'path', 'line', 'databaseId', 'author{login}', 'createdAt', 'url']) {
    assert.ok(query.includes(needle), `query missing ${needle}`);
  }
});

// -- mapReviewThreads --

function threadsFixture() {
  return {
    data: {
      repository: {
        pullRequest: {
          reviewThreads: {
            nodes: [
              {
                id: 'THREAD_RESOLVED',
                isResolved: true,
                isOutdated: false,
                path: 'a.go',
                line: 10,
                comments: {
                  nodes: [
                    { id: 'RC_1a', databaseId: 101, author: { login: 'alice' }, bodyHTML: '<p>looks fine now</p>', createdAt: '2026-01-01T00:00:00Z', url: 'https://x/1' },
                    { id: 'RC_1b', databaseId: 102, author: { login: 'bob' }, bodyHTML: '<p>thanks</p>', createdAt: '2026-01-01T01:00:00Z', url: 'https://x/2' },
                  ],
                },
              },
              {
                id: 'THREAD_UNRESOLVED',
                isResolved: false,
                isOutdated: true,
                path: 'b.go',
                line: 42,
                comments: {
                  nodes: [
                    { id: 'RC_2a', databaseId: 201, author: { login: 'carol' }, bodyHTML: '<p>fix this</p><script>alert(1)</script>', createdAt: '2026-01-02T00:00:00Z', url: 'https://x/3' },
                    { id: 'RC_2b', databaseId: 202, author: { login: 'dave' }, bodyHTML: '<p>on it</p>', createdAt: '2026-01-02T01:00:00Z', url: 'https://x/4' },
                  ],
                },
              },
            ],
          },
        },
      },
    },
  };
}

test('mapReviewThreads: maps id/resolved/outdated/path/line and sorts unresolved before resolved', () => {
  const out = mapReviewThreads(threadsFixture());
  assert.equal(out.length, 2);
  assert.equal(out[0].id, 'THREAD_UNRESOLVED');
  assert.equal(out[0].resolved, false);
  assert.equal(out[0].outdated, true);
  assert.equal(out[0].path, 'b.go');
  assert.equal(out[0].line, 42);
  assert.equal(out[1].id, 'THREAD_RESOLVED');
  assert.equal(out[1].resolved, true);
  assert.equal(out[1].outdated, false);
});

test('mapReviewThreads: comments carry id as the stringified databaseId, nodeId, actor, sanitised bodyHtml, createdAt, url', () => {
  const out = mapReviewThreads(threadsFixture());
  const resolved = out.find((t) => t.id === 'THREAD_RESOLVED');
  assert.deepEqual(resolved.comments.map((c) => c.id), ['101', '102']);
  assert.deepEqual(resolved.comments.map((c) => c.nodeId), ['RC_1a', 'RC_1b']);
  assert.equal(resolved.comments[0].actor, 'alice');
  assert.equal(resolved.comments[0].bodyHtml, '<p>looks fine now</p>');
  assert.equal(resolved.comments[0].createdAt, '2026-01-01T00:00:00Z');
  assert.equal(resolved.comments[0].url, 'https://x/1');
});

test('mapReviewThreads: within a group, threads sort by their first comment createdAt', () => {
  const fixture = threadsFixture();
  // Two unresolved threads, second created earlier than the first in document order.
  fixture.data.repository.pullRequest.reviewThreads.nodes = [
    { id: 'LATER', isResolved: false, isOutdated: false, path: 'c.go', line: 1, comments: { nodes: [{ id: 'n1', databaseId: 1, author: { login: 'x' }, bodyHTML: '<p>later</p>', createdAt: '2026-02-02T00:00:00Z', url: 'u1' }] } },
    { id: 'EARLIER', isResolved: false, isOutdated: false, path: 'd.go', line: 2, comments: { nodes: [{ id: 'n2', databaseId: 2, author: { login: 'y' }, bodyHTML: '<p>earlier</p>', createdAt: '2026-02-01T00:00:00Z', url: 'u2' }] } },
  ];
  const out = mapReviewThreads(fixture);
  assert.deepEqual(out.map((t) => t.id), ['EARLIER', 'LATER']);
});

test('teeth: a <script> smuggled into a review comment bodyHTML never survives mapReviewThreads', () => {
  const out = mapReviewThreads(threadsFixture());
  const unresolved = out.find((t) => t.id === 'THREAD_UNRESOLVED');
  const evilComment = unresolved.comments.find((c) => c.id === '201');
  assert.ok(!/<script/i.test(evilComment.bodyHtml), `script tag survived: ${evilComment.bodyHtml}`);
});

test('mapReviewThreads: an empty/missing nodes array maps to []', () => {
  assert.deepEqual(mapReviewThreads({ data: { repository: { pullRequest: { reviewThreads: { nodes: [] } } } } }), []);
  assert.deepEqual(mapReviewThreads({ data: { repository: { pullRequest: null } } }), []);
  assert.deepEqual(mapReviewThreads({}), []);
});

// -- resolveThreadArgs --

test('resolveThreadArgs: resolved true builds the resolveReviewThread mutation, false the unresolveReviewThread one', () => {
  const resolveArgs = resolveThreadArgs({ threadId: 'THREAD_1', resolved: true });
  assert.deepEqual(resolveArgs.slice(0, 2), ['api', 'graphql']);
  const resolveQuery = resolveArgs[resolveArgs.indexOf('-f') + 1];
  assert.match(resolveQuery, /^query=/);
  assert.ok(resolveQuery.includes('resolveReviewThread'));
  assert.ok(!resolveQuery.includes('unresolveReviewThread'));
  assert.ok(resolveArgs.includes('threadId=THREAD_1'));

  const unresolveArgs = resolveThreadArgs({ threadId: 'THREAD_1', resolved: false });
  const unresolveQuery = unresolveArgs[unresolveArgs.indexOf('-f') + 1];
  assert.ok(unresolveQuery.includes('unresolveReviewThread'));
});

// -- replyArgs --

test('replyArgs: POST pulls/<n>/comments/<commentId>/replies with the html accept header, body on stdin', () => {
  const args = replyArgs({ repo: REPO, number: NUMBER, commentId: '101' });
  assert.deepEqual(args, ['api', '-X', 'POST', 'repos/o/r/pulls/7/comments/101/replies', '-H', 'Accept: application/vnd.github.html+json', '--input', '-']);
});

// -- prTitleArgs --

test('prTitleArgs: gh pr edit <n> --repo o/r --title <title>', () => {
  assert.deepEqual(prTitleArgs({ repo: REPO, number: NUMBER, title: 'New title' }), ['pr', 'edit', '7', '--repo', 'o/r', '--title', 'New title']);
});

// -- rerunFailedArgs --

function checkRunsFixture(runs) {
  return { total_count: runs.length, check_runs: runs };
}

test('rerunFailedArgs: dedupes two failed jobs of the same workflow run into one rerun-failed-jobs argv', () => {
  const mapped = mapCheckRuns(
    checkRunsFixture([
      { id: 1, name: 'job-a', status: 'completed', conclusion: 'failure', html_url: 'https://github.com/o/r/actions/runs/555/job/1', app: { slug: 'gha' } },
      { id: 2, name: 'job-b', status: 'completed', conclusion: 'failure', html_url: 'https://github.com/o/r/actions/runs/555/job/2', app: { slug: 'gha' } },
      { id: 3, name: 'job-c', status: 'completed', conclusion: 'success', html_url: 'https://github.com/o/r/actions/runs/555/job/3', app: { slug: 'gha' } },
    ])
  );
  const args = rerunFailedArgs({ repo: REPO, runs: mapped.runs });
  assert.deepEqual(args, [['api', '-X', 'POST', 'repos/o/r/actions/runs/555/rerun-failed-jobs']]);
});

test('rerunFailedArgs: distinct failed workflow runs each get their own rerun-failed-jobs argv', () => {
  const mapped = mapCheckRuns(
    checkRunsFixture([
      { id: 1, name: 'job-a', status: 'completed', conclusion: 'failure', html_url: 'https://github.com/o/r/actions/runs/111/job/1', app: { slug: 'gha' } },
      { id: 2, name: 'job-b', status: 'completed', conclusion: 'failure', html_url: 'https://github.com/o/r/actions/runs/222/job/2', app: { slug: 'gha' } },
    ])
  );
  const args = rerunFailedArgs({ repo: REPO, runs: mapped.runs });
  assert.deepEqual(args.sort(), [
    ['api', '-X', 'POST', 'repos/o/r/actions/runs/111/rerun-failed-jobs'],
    ['api', '-X', 'POST', 'repos/o/r/actions/runs/222/rerun-failed-jobs'],
  ].sort());
});

test('rerunFailedArgs: a failed run whose url has no actions/runs/<id> falls back to check-runs/<id>/rerequest', () => {
  const mapped = mapCheckRuns(checkRunsFixture([{ id: 777, name: 'external-check', status: 'completed', conclusion: 'failure', html_url: 'https://example.com/not-a-run-url', app: { slug: 'some-app' } }]));
  const args = rerunFailedArgs({ repo: REPO, runs: mapped.runs });
  assert.deepEqual(args, [['api', '-X', 'POST', 'repos/o/r/check-runs/777/rerequest']]);
});

test('rerunFailedArgs: no failed runs returns []', () => {
  const mapped = mapCheckRuns(checkRunsFixture([{ id: 1, name: 'ok', status: 'completed', conclusion: 'success', html_url: 'https://github.com/o/r/actions/runs/1/job/1', app: { slug: 'gha' } }]));
  assert.deepEqual(rerunFailedArgs({ repo: REPO, runs: mapped.runs }), []);
});

// -- wrappers (recording fake execFileImpl) --

function recordingExec(responses) {
  const calls = [];
  const impl = (bin, args, opts, cb) => {
    calls.push({ bin, args, opts });
    const hit = responses.find((r) => r.match(args));
    if (!hit) return cb(new Error(`no fixture for args ${JSON.stringify(args)}`));
    cb(null, hit.stdout, '');
  };
  return { impl, calls };
}

test('reviewThreads(): fetches the graphql query and maps it', async () => {
  const { impl, calls } = recordingExec([{ match: (a) => a.includes('graphql'), stdout: JSON.stringify(threadsFixture()) }]);
  const out = await reviewThreads({ repo: REPO, number: NUMBER, execFileImpl: impl });
  assert.equal(calls.length, 1);
  assert.equal(out.length, 2);
  assert.equal(out[0].id, 'THREAD_UNRESOLVED');
});

test('setThreadResolved(): resolves to {ok:true}', async () => {
  const exec = (bin, args, opts, cb) => cb(null, '{}', '');
  assert.deepEqual(await setThreadResolved({ threadId: 'T1', resolved: true, execFileImpl: exec }), { ok: true });
  assert.deepEqual(await setThreadResolved({ threadId: 'T1', resolved: false, execFileImpl: exec }), { ok: true });
});

test('replyToThread(): POSTs the body on stdin and resolves to the mapped comment', async () => {
  let seenOpts;
  const impl = (bin, args, opts, cb) => {
    seenOpts = opts;
    assert.deepEqual(args, replyArgs({ repo: REPO, number: NUMBER, commentId: '101' }));
    cb(null, JSON.stringify({ id: 999, user: { login: 'me', avatar_url: 'https://x/me.png' }, body_html: '<p>reply</p>', created_at: '2026-02-02T00:00:00Z' }), '');
  };
  const entry = await replyToThread({ repo: REPO, number: NUMBER, commentId: '101', body: 'reply', execFileImpl: impl });
  assert.ok(seenOpts && seenOpts.input != null, 'the reply body must be handed through options.input, not argv');
  assert.deepEqual(JSON.parse(String(seenOpts.input)), { body: 'reply' });
  assert.equal(entry.kind, 'comment');
  assert.equal(entry.actor, 'me');
  assert.equal(entry.bodyHtml, '<p>reply</p>');
  assert.equal(entry.id, '999');
});

test('setPrTitle(): resolves to {ok:true}', async () => {
  const exec = (bin, args, opts, cb) => {
    assert.deepEqual(args, prTitleArgs({ repo: REPO, number: NUMBER, title: 'New title' }));
    cb(null, '', '');
  };
  assert.deepEqual(await setPrTitle({ repo: REPO, number: NUMBER, title: 'New title', execFileImpl: exec }), { ok: true });
});

test('rerunFailedChecks(): fires one POST per rerunFailedArgs entry, resolves {ok:true, reruns:n}', async () => {
  const mapped = mapCheckRuns(
    checkRunsFixture([
      { id: 1, name: 'job-a', status: 'completed', conclusion: 'failure', html_url: 'https://github.com/o/r/actions/runs/111/job/1', app: { slug: 'gha' } },
      { id: 2, name: 'job-b', status: 'completed', conclusion: 'failure', html_url: 'https://github.com/o/r/actions/runs/222/job/2', app: { slug: 'gha' } },
    ])
  );
  let calls = 0;
  const exec = (bin, args, opts, cb) => { calls++; cb(null, '', ''); };
  const out = await rerunFailedChecks({ repo: REPO, runs: mapped.runs, execFileImpl: exec });
  assert.equal(calls, 2);
  assert.deepEqual(out, { ok: true, reruns: 2 });
});

test('rerunFailedChecks(): no failed runs makes no calls and resolves {ok:true, reruns:0}', async () => {
  const mapped = mapCheckRuns(checkRunsFixture([{ id: 1, name: 'ok', status: 'completed', conclusion: 'success', html_url: 'https://github.com/o/r/actions/runs/1/job/1', app: { slug: 'gha' } }]));
  let calls = 0;
  const exec = (bin, args, opts, cb) => { calls++; cb(null, '', ''); };
  const out = await rerunFailedChecks({ repo: REPO, runs: mapped.runs, execFileImpl: exec });
  assert.equal(calls, 0);
  assert.deepEqual(out, { ok: true, reruns: 0 });
});
