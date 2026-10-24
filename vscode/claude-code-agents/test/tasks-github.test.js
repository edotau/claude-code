'use strict';
// Unit tests for the GitHub `gh`-CLI read path (lib/tasks-github.js): check folding, merge
// readiness, row mappers, argv builders, error classification, listWorkItems paging/fan-out and
// authStatus, against test/fixtures/github/ (real gh output shapes).
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {
  PER_REPO_FETCH_LIMIT,
  CROSS_REPO_DISPLAY_LIMIT,
  summarizeChecks,
  mergeState,
  mapPullRequest,
  mapIssue,
  issueSearchArgs,
  prListArgs,
  countArgs,
  classifyGhError,
  listWorkItems,
  authStatus,
} = require('../lib/tasks-github');

const FIXTURE_DIR = path.join(__dirname, 'fixtures', 'github');
function fixture(name) {
  return JSON.parse(fs.readFileSync(path.join(FIXTURE_DIR, `${name}.json`), 'utf8'));
}

const REPO = { owner: 'acme', name: 'widgets' };
const QUERY = { scope: 'all', state: null, assignee: null, author: null, labels: [], freeText: '', draft: false };

// -- summarizeChecks --

test('summarizeChecks: #33127 (3 IN_PROGRESS + 3 SUCCESS) folds to in_progress', () => {
  const out = summarizeChecks(fixture('pr-list')[0].statusCheckRollup);
  assert.equal(out.state, 'in_progress');
  assert.equal(out.label, '3 running');
  assert.deepEqual(out.counts, { success: 3, failed: 0, pending: 3, skipped: 0 });
  assert.equal('url' in out, false);
});

test('summarizeChecks: #33126 (5 SKIPPED + 1 SUCCESS) folds to success', () => {
  const out = summarizeChecks(fixture('pr-list')[1].statusCheckRollup);
  assert.equal(out.state, 'success');
  assert.equal(out.label, '1 passed');
  assert.deepEqual(out.counts, { success: 1, failed: 0, pending: 0, skipped: 5 });
});

test('summarizeChecks: #33123 (4 SUCCESS + 2 SKIPPED) folds to success', () => {
  const out = summarizeChecks(fixture('pr-list')[2].statusCheckRollup);
  assert.equal(out.state, 'success');
  assert.equal(out.label, '4 passed');
  assert.deepEqual(out.counts, { success: 4, failed: 0, pending: 0, skipped: 2 });
});

test('summarizeChecks: #33122 (5 SKIPPED + 1 SUCCESS) folds to success', () => {
  const out = summarizeChecks(fixture('pr-list')[3].statusCheckRollup);
  assert.equal(out.state, 'success');
  assert.equal(out.label, '1 passed');
  assert.deepEqual(out.counts, { success: 1, failed: 0, pending: 0, skipped: 5 });
});

test('summarizeChecks: an empty or missing rollup folds to none / "No checks"', () => {
  for (const rollup of [[], undefined, null]) {
    const out = summarizeChecks(rollup);
    assert.equal(out.state, 'none');
    assert.equal(out.label, 'No checks');
    assert.deepEqual(out.counts, { success: 0, failed: 0, pending: 0, skipped: 0 });
  }
});

test('summarizeChecks: one FAILURE folds to failed and surfaces its detailsUrl', () => {
  const out = summarizeChecks([
    { status: 'COMPLETED', conclusion: 'SUCCESS', detailsUrl: 'https://ci.example/ok' },
    { status: 'COMPLETED', conclusion: 'FAILURE', detailsUrl: 'https://ci.example/bad' },
  ]);
  assert.equal(out.state, 'failed');
  assert.match(out.label, /1 failing/);
  assert.deepEqual(out.counts, { success: 1, failed: 1, pending: 0, skipped: 0 });
  assert.equal(out.url, 'https://ci.example/bad');
});

// -- mergeState --

test('mergeState: merged and closed are terminal regardless of the item', () => {
  assert.deepEqual(mergeState({}, 'merged'), { state: 'merged', label: 'Merged' });
  assert.deepEqual(mergeState({ mergeStateStatus: 'CLEAN', mergeable: 'MERGEABLE' }, 'merged'), { state: 'merged', label: 'Merged' });
  assert.deepEqual(mergeState({}, 'closed'), { state: 'closed', label: 'Closed' });
});

test('mergeState: a draft is blocked with the Draft label', () => {
  assert.deepEqual(mergeState({ mergeStateStatus: 'CLEAN', mergeable: 'MERGEABLE' }, 'draft'), { state: 'blocked', label: 'Draft' });
});

test('mergeState: an open PR with UNSTABLE checks is blocked with "Checks failing"', () => {
  assert.deepEqual(
    mergeState({ mergeStateStatus: 'UNSTABLE', mergeable: 'MERGEABLE' }, 'open'),
    { state: 'blocked', label: 'Checks failing' }
  );
});

test('mergeState: an open PR with CLEAN checks is ready', () => {
  assert.deepEqual(
    mergeState({ mergeStateStatus: 'CLEAN', mergeable: 'MERGEABLE' }, 'open'),
    { state: 'ready', label: 'Ready' }
  );
});

test('mergeState: a CONFLICTING mergeable reports conflicts', () => {
  assert.deepEqual(
    mergeState({ mergeStateStatus: 'DIRTY', mergeable: 'CONFLICTING' }, 'open'),
    { state: 'conflict', label: 'Conflicts' }
  );
});

// -- mapPullRequest --

test('mapPullRequest: #33127 maps id/title/state/branches/diff stats and populated checks+merge', () => {
  const raw = fixture('pr-list')[0];
  const row = mapPullRequest(raw, REPO);
  assert.equal(row.id, 'github:acme/widgets:pr:33127');
  assert.equal(row.number, 33127);
  assert.equal(row.title, 'refactor(renderer): group GitHub task read and refresh owner');
  assert.equal(row.state, 'open');
  assert.equal(row.url, 'https://github.com/acme/widgets/pull/33127');
  assert.deepEqual(row.repo, { owner: 'acme', name: 'widgets', label: 'acme/widgets' });
  assert.equal(row.author, 'dev1');
  assert.equal(row.branch, 'dev/module-organization-github-task-reads');
  assert.equal(row.baseBranch, 'main');
  assert.equal(row.additions, 114);
  assert.equal(row.deletions, 115);
  assert.equal(row.changedFiles, 45);
  assert.equal(typeof row.checks, 'object');
  assert.notEqual(row.checks, null);
  assert.equal(typeof row.merge, 'object');
  assert.notEqual(row.merge, null);
});

test('mapPullRequest: #33126 (isDraft) maps state to draft', () => {
  const raw = fixture('pr-list')[1];
  const row = mapPullRequest(raw, REPO);
  assert.equal(row.id, 'github:acme/widgets:pr:33126');
  assert.equal(row.number, 33126);
  assert.equal(row.state, 'draft');
  assert.equal(row.url, 'https://github.com/acme/widgets/pull/33126');
  assert.equal(row.author, 'dev1');
  assert.equal(row.branch, 'dev/wsl-host-bridge-spares');
  assert.equal(row.baseBranch, 'main');
  assert.equal(row.additions, 146);
  assert.equal(row.deletions, 16);
  assert.equal(row.changedFiles, 6);
  assert.deepEqual(row.merge, { state: 'blocked', label: 'Draft' });
  assert.notEqual(row.checks, null);
});

// -- mapIssue --

test('mapIssue: #33124 maps the REST search shape with labels and null checks/merge', () => {
  const raw = fixture('issue-search')[1];
  const row = mapIssue(raw, REPO);
  assert.equal(row.id, 'github:acme/widgets:issue:33124');
  assert.equal(row.number, 33124);
  assert.equal(row.title, '[Bug]: Headless server (Electron host): projects cannot be s');
  assert.equal(row.state, 'open');
  assert.deepEqual(row.labels, ['bug', 'os:linux']);
  assert.equal(row.author, 'dev2');
  assert.equal(row.url, 'https://github.com/acme/widgets/issues/33124');
  assert.deepEqual(row.repo, { owner: 'acme', name: 'widgets', label: 'acme/widgets' });
  assert.equal(row.checks, null);
  assert.equal(row.merge, null);
});

// -- argv builders --

test('issueSearchArgs: gh api search/issues argv with the repo/issue/open qualifiers', () => {
  const args = issueSearchArgs({ repo: REPO, query: QUERY, page: 3, limit: 10 });
  assert.equal(args[0], 'api');
  const url = args.find((a) => a.startsWith('search/issues?'));
  assert.ok(url, 'expected a search/issues URL argument');
  assert.ok(url.includes('per_page=10'));
  assert.ok(url.includes('page=3'));
  const q = decodeURIComponent(url.split('q=')[1].split('&')[0]);
  assert.ok(q.includes('repo:acme/widgets'));
  assert.ok(q.includes('is:issue'));
});

test('issueSearchArgs: state open adds the is:open qualifier', () => {
  const args = issueSearchArgs({ repo: REPO, query: { ...QUERY, state: 'open' }, page: 1, limit: 10 });
  const url = args.find((a) => a.startsWith('search/issues?'));
  const q = decodeURIComponent(url.split('q=')[1].split('&')[0]);
  assert.ok(q.includes('is:open'));
});

test('prListArgs: gh pr list argv with the repo slug, PR qualifiers and a page offset', () => {
  const { args, offset } = prListArgs({ repo: REPO, query: { ...QUERY, state: 'open' }, page: 3, limit: 10 });
  assert.equal(args[0], 'pr');
  assert.equal(args[1], 'list');
  assert.ok(args.includes('--repo'));
  assert.ok(args.includes('acme/widgets'));
  assert.ok(args.includes('--limit'));
  assert.equal(args[args.indexOf('--limit') + 1], '30');
  const search = args[args.indexOf('--search') + 1];
  assert.ok(search.includes('is:pr'));
  assert.ok(search.includes('is:open'));
  assert.ok(search.includes('sort:created-desc'));
  assert.equal(offset, (3 - 1) * 10);
});

test('countArgs: gh api search/issues per_page=1 argv with the kind qualifiers', () => {
  const issueArgv = countArgs({ repo: REPO, query: QUERY, kind: 'issue' });
  assert.equal(issueArgv[0], 'api');
  const issueUrl = issueArgv.find((a) => a.startsWith('search/issues?'));
  assert.ok(issueUrl.includes('per_page=1'));
  assert.ok(decodeURIComponent(issueUrl.split('q=')[1].split('&')[0]).includes('is:issue'));

  const prArgv = countArgs({ repo: REPO, query: QUERY, kind: 'pr' });
  const prUrl = prArgv.find((a) => a.startsWith('search/issues?'));
  const prQ = decodeURIComponent(prUrl.split('q=')[1].split('&')[0]);
  assert.ok(prQ.includes('is:pr'));
  assert.ok(prQ.includes('repo:acme/widgets'));
});

// -- classifyGhError --

test('classifyGhError: maps stderr classes to human messages', () => {
  assert.match(classifyGhError({ stderr: 'API rate limit exceeded for user' }), /rate limit/i);
  assert.match(classifyGhError({ stderr: 'gh: not logged in to github.com' }), /sign|logged in|auth login/i);
  assert.match(classifyGhError({ stderr: 'HTTP 404: Not Found' }), /not found/i);
});

test('classifyGhError: an ENOENT error means gh is not installed', () => {
  const err = Object.assign(new Error('spawn gh ENOENT'), { code: 'ENOENT' });
  assert.match(classifyGhError(err), /not installed/i);
});

test('classifyGhError: a generic error falls back to the first line of its message', () => {
  assert.equal(classifyGhError(new Error('something went wrong\n  at stack')), 'something went wrong');
});

// -- listWorkItems --

// ghExec calls execFileImpl(cmd, args, options, cb) with cb(err, stdout, stderr).
function ghFake({ issuesError } = {}) {
  const calls = [];
  const execFileImpl = (cmd, args, opts, cb) => {
    calls.push(args);
    if (args[0] === 'pr') return cb(null, JSON.stringify(fixture('pr-list')));
    const url = args.find((a) => typeof a === 'string' && a.includes('search/issues'));
    if (args[0] === 'api' && url && url.includes('per_page=1')) return cb(null, '40\n');
    if (args[0] === 'api' && url) {
      if (issuesError) return cb(issuesError);
      return cb(null, JSON.stringify(fixture('issue-search')));
    }
    return cb(new Error(`unexpected gh call: ${args.join(' ')}`));
  };
  return { execFileImpl, calls };
}

test('listWorkItems: page 2 of PRs is the offset slice, totalPages comes from the count', async () => {
  const { execFileImpl } = ghFake();
  const out = await listWorkItems({ repos: [REPO], query: QUERY, page: 2, limit: 2, execFileImpl });
  const prNumbers = out.items.filter((i) => i.type === 'pr').map((i) => i.number);
  assert.deepEqual(prNumbers, [33123, 33122]); // slice(2, 4) of the 4-item fixture
  assert.ok(out.items.some((i) => i.type === 'issue' && i.number === 33125));
  assert.equal(out.totalPages, Math.ceil(40 / 2));
  assert.deepEqual(out.errors, []);
});

test('listWorkItems: a failing issue search becomes one errors entry and PRs still come through', async () => {
  const { execFileImpl } = ghFake({ issuesError: new Error('boom') });
  const out = await listWorkItems({ repos: [REPO], query: QUERY, page: 2, limit: 2, execFileImpl });
  assert.equal(out.errors.length, 1);
  assert.equal(out.errors[0].kind, 'issues');
  assert.equal(out.errors[0].repo, 'acme/widgets');
  const prNumbers = out.items.filter((i) => i.type === 'pr').map((i) => i.number);
  assert.deepEqual(prNumbers, [33123, 33122]);
  assert.equal(out.items.some((i) => i.type === 'issue'), false);
});

test('listWorkItems: scope=pr never issues an issue-search call', async () => {
  const { execFileImpl, calls } = ghFake();
  const out = await listWorkItems({
    repos: [REPO],
    query: { ...QUERY, scope: 'pr' },
    page: 1,
    limit: PER_REPO_FETCH_LIMIT,
    execFileImpl,
  });
  assert.ok(calls.length > 0);
  for (const args of calls) {
    for (const a of args) {
      assert.equal(String(a).includes('is:issue'), false);
      assert.equal(String(a).includes('is%3Aissue'), false);
    }
  }
  assert.ok(calls.some((args) => args[0] === 'pr'));
  assert.ok(out.items.every((i) => i.type === 'pr'));
});

test('listWorkItems: two repos sort by updatedAt descending and stay under the display cap', async () => {
  const { execFileImpl } = ghFake();
  const repos = [REPO, { owner: 'acme', name: 'gadgets' }];
  const out = await listWorkItems({ repos, query: QUERY, page: 1, limit: PER_REPO_FETCH_LIMIT, execFileImpl });
  assert.ok(out.items.length <= CROSS_REPO_DISPLAY_LIMIT);
  assert.equal(out.items.length, 2 * (fixture('pr-list').length + fixture('issue-search').length));
  for (let i = 1; i < out.items.length; i++) {
    assert.ok(Date.parse(out.items[i - 1].updatedAt) >= Date.parse(out.items[i].updatedAt));
  }
});

// -- authStatus --

test('authStatus: a successful gh api user resolves ok with the login', async () => {
  const execFileImpl = (cmd, args, opts, cb) => cb(null, 'octocat\n');
  assert.deepEqual(await authStatus(execFileImpl), { ok: true, login: 'octocat' });
});

test('authStatus: an ENOENT failure reports gh as not installed', async () => {
  const execFileImpl = (cmd, args, opts, cb) => cb(Object.assign(new Error('spawn gh ENOENT'), { code: 'ENOENT' }));
  const out = await authStatus(execFileImpl);
  assert.equal(out.ok, false);
  assert.match(out.message, /not installed/i);
});
