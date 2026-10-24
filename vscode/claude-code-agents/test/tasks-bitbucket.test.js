'use strict';
// Unit tests for the Bitbucket Data Center tasks adapter: dcState, endpointFor, repoFromUrl,
// mapRow, filterRows and list, against test/fixtures/bitbucket-pr.fixture.json (an already-mapped
// bitbucket-client row) and the raw Data Center payloads under test/fixtures/bitbucket/.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { dcState, endpointFor, repoFromUrl, mapRow, filterRows, list } = require('../lib/tasks-bitbucket');
const { detectJiraIssueKey } = require('../lib/tasks-model');
const { mapPullRequest } = require('../lib/bitbucket-client');

const FIXTURE_ROW = JSON.parse(
  fs.readFileSync(path.join(__dirname, 'fixtures', 'bitbucket-pr.fixture.json'), 'utf8')
)[0];

function rawFixture(name) {
  return JSON.parse(fs.readFileSync(path.join(__dirname, 'fixtures', 'bitbucket', `${name}.json`), 'utf8'));
}

// -- dcState --

test('dcState: query state maps to the DC state param', () => {
  assert.equal(dcState({ state: 'merged' }), 'MERGED');
  assert.equal(dcState({ state: 'closed' }), 'DECLINED');
  assert.equal(dcState({ state: 'all' }), 'ALL');
  assert.equal(dcState({ state: null }), 'OPEN');
  assert.equal(dcState({ state: 'open' }), 'OPEN');
  assert.equal(dcState({}), 'OPEN');
});

// -- endpointFor --

test('endpointFor: review-requested:@me routes to the dashboard as REVIEWER', () => {
  assert.deepEqual(endpointFor({ reviewRequested: '@me' }), { kind: 'dashboard', role: 'REVIEWER' });
});

test('endpointFor: author:@me routes to the dashboard as AUTHOR', () => {
  assert.deepEqual(endpointFor({ author: '@me' }), { kind: 'dashboard', role: 'AUTHOR' });
});

test('endpointFor: a non-@me author or an empty query routes to the repo PR list', () => {
  assert.deepEqual(endpointFor({ author: 'someone-else' }), { kind: 'repo' });
  assert.deepEqual(endpointFor({}), { kind: 'repo' });
});

test('endpointFor: review-requested:@me wins over author:@me (checked first)', () => {
  assert.deepEqual(endpointFor({ reviewRequested: '@me', author: '@me' }), { kind: 'dashboard', role: 'REVIEWER' });
});

// -- repoFromUrl --

test('repoFromUrl: parses the fixture row\'s DC PR URL', () => {
  assert.deepEqual(repoFromUrl(FIXTURE_ROW.url), { project: 'DIGITALBTD', repo: 'digitalbtd-portal-frontend' });
});

test('repoFromUrl: a URL without the /projects/.../repos/.../ shape is null', () => {
  assert.equal(repoFromUrl('https://bitbucket.biscrum.com/dashboard'), null);
  assert.equal(repoFromUrl(''), null);
});

test('repoFromUrl: percent-encoded segments are decoded and the project uppercased', () => {
  assert.deepEqual(
    repoFromUrl('https://bitbucket.biscrum.com/projects/my%20proj/repos/my%20repo/pull-requests/1'),
    { project: 'MY PROJ', repo: 'my repo' }
  );
});

// -- mapRow --

test('mapRow: the fixture row maps to the row contract', () => {
  const row = mapRow(FIXTURE_ROW);
  const jiraKey = detectJiraIssueKey({ branch: FIXTURE_ROW.from_branch, title: FIXTURE_ROW.title });
  assert.equal(row.source, 'bitbucket');
  assert.equal(row.id, 'bitbucket:DIGITALBTD/digitalbtd-portal-frontend:pr:1662');
  assert.equal(row.number, 1662);
  assert.equal(row.type, 'pr');
  // The fixture's state is the literal 'OPEN'; mapRow only lowercases 'merged'/'declined', so this is 'open'.
  assert.equal(row.state, 'open');
  assert.deepEqual(row.repo, { owner: 'DIGITALBTD', name: 'digitalbtd-portal-frontend', label: 'DIGITALBTD/digitalbtd-portal-frontend' });
  assert.deepEqual(row.reviewers, [
    { name: 'Dev 2', status: 'APPROVED' },
    { name: 'Dev 3', status: 'PENDING' },
    { name: 'Dev 4', status: 'PENDING' },
  ]);
  // Fixture build is {url, state:'success'} with no `successful` count, so the label falls back to 0.
  assert.equal(row.checks.state, 'success');
  assert.equal(row.checks.label, '0 passed');
  assert.equal(row.checks.url, 'https://jenkins.example.com/job/digitalbtd-cd/job/digitalbtd-cd-portal-frontend-10051/1/');
  assert.deepEqual(row.merge, { state: 'unknown', label: 'Open in Bitbucket' });
  assert.equal(row.jiraKey, jiraKey);
  assert.equal(row.jiraUrl, null);
});

test('mapRow: jiraBaseUrl produces a /browse/ jiraUrl with a trailing slash stripped', () => {
  const jiraKey = detectJiraIssueKey({ branch: FIXTURE_ROW.from_branch, title: FIXTURE_ROW.title });
  const row = mapRow(FIXTURE_ROW, { jiraBaseUrl: 'https://jira.example.com/' });
  assert.equal(row.jiraUrl, `https://jira.example.com/browse/${jiraKey}`);
});

// -- filterRows --

function miniRow(overrides) {
  return {
    title: 'some pull request',
    branch: 'feature/x',
    number: 1,
    author: 'alice',
    reviewers: [],
    jiraKey: null,
    ...overrides,
  };
}

test('filterRows: a non-@me author filters case-insensitively by substring', () => {
  const rows = [miniRow({ author: 'Alice Smith' }), miniRow({ author: 'Bob' }), miniRow({ author: 'ALICE Jones' })];
  assert.equal(filterRows(rows, { author: 'alice' }).length, 2);
  assert.equal(filterRows(rows, { author: 'bob' }).length, 1);
});

test('filterRows: author:@me does not filter (the server already scoped the results)', () => {
  const rows = [miniRow({ author: 'Alice' }), miniRow({ author: 'Bob' })];
  assert.equal(filterRows(rows, { author: '@me' }).length, 2);
});

test('filterRows: a non-@me reviewRequested filters on reviewer name, case-insensitive substring', () => {
  const rows = [
    miniRow({ reviewers: [{ name: 'Carol' }] }),
    miniRow({ reviewers: [{ name: 'Dave' }, { name: 'CAROLE King' }] }),
    miniRow({ reviewers: [{ name: 'Eve' }] }),
  ];
  const out = filterRows(rows, { reviewRequested: 'carol' });
  assert.equal(out.length, 2);
  assert.equal(filterRows(rows, { reviewRequested: '@me' }).length, 3);
});

test('filterRows: freeText requires ALL words to match across title/branch/#number/jiraKey/author', () => {
  const rows = [
    miniRow({ title: 'Add Excel export', branch: 'DIGITALBTD-10051/frontend', number: 1662, author: 'dev-1', jiraKey: 'DIGITALBTD-10051' }),
    miniRow({ title: 'Excel chart', branch: 'feature/chart', number: 2, author: 'bob' }),
  ];
  assert.equal(filterRows(rows, { freeText: 'excel digitalbtd-10051' }).length, 1);
  assert.equal(filterRows(rows, { freeText: 'excel bob' }).length, 1);
  assert.equal(filterRows(rows, { freeText: '#1662 excel' }).length, 1);
  assert.equal(filterRows(rows, { freeText: 'excel nosuchword' }).length, 0);
});

test('filterRows: quotes are stripped from freeText before word-splitting', () => {
  const rows = [miniRow({ title: 'Add Excel export' })];
  assert.equal(filterRows(rows, { freeText: '"add excel"' }).length, 1);
});

// -- list --

function makeRaw(overrides) {
  const raw = JSON.parse(JSON.stringify(rawFixture('pr-by-id-open')));
  return { ...raw, ...overrides };
}

function fakeClient({ dashboardImpl, pullRequestsImpl, calls }) {
  return {
    dashboard: async (role, state) => {
      calls.push({ method: 'dashboard', args: [role, state] });
      return dashboardImpl ? dashboardImpl(role, state) : [];
    },
    pullRequests: async (ref, state) => {
      calls.push({ method: 'pullRequests', args: [ref, state] });
      return pullRequestsImpl(ref, state);
    },
    listWithBuilds: async (rawRows) => rawRows.map((raw) => mapPullRequest(raw, null)),
  };
}

test('list: review-requested:@me routes to client.dashboard with REVIEWER + the DC state', async () => {
  const calls = [];
  const client = fakeClient({ calls, dashboardImpl: async () => [makeRaw({})] });
  const result = await list({ client, query: { reviewRequested: '@me' }, repos: [], page: 1, limit: 36 });
  assert.deepEqual(calls[0], { method: 'dashboard', args: ['REVIEWER', 'OPEN'] });
  assert.equal(calls.filter((c) => c.method === 'pullRequests').length, 0);
  assert.equal(result.items.length, 1);
  assert.equal(result.errors.length, 0);
});

test('list: a repo query fans out over repos and a rejected repo lands in errors without dropping the other', async () => {
  const calls = [];
  const refA = { host: 'h', project: 'PROJA', repo: 'repo-a' };
  const refB = { host: 'h', project: 'PROJB', repo: 'repo-b' };
  const client = fakeClient({
    calls,
    pullRequestsImpl: async (ref) => {
      if (ref.repo === 'repo-b') throw new Error('boom');
      return [makeRaw({ id: 1, author: { user: { displayName: 'someone dev' } } })];
    },
  });
  const result = await list({ client, query: { author: 'someone' }, repos: [refA, refB], page: 1, limit: 36 });
  assert.equal(calls.filter((c) => c.method === 'pullRequests').length, 2);
  assert.equal(result.errors.length, 1);
  assert.equal(result.errors[0].repo, 'PROJB/repo-b');
  assert.equal(result.errors[0].message, 'boom');
  assert.equal(result.failedRepos, 1);
  assert.equal(result.items.length, 1);
  assert.equal(result.items[0].number, 1);
});

test('list: paginates the filtered rows by limit', async () => {
  const calls = [];
  const raw = [1, 2, 3, 4, 5].map((id) => makeRaw({ id, updatedDate: 1792622035057 + id * 1000 }));
  const client = fakeClient({ calls, pullRequestsImpl: async () => raw });
  const ref = { host: 'h', project: 'PROJ', repo: 'repo' };
  const result = await list({ client, query: {}, repos: [ref], page: 2, limit: 2 });
  assert.equal(result.totalPages, Math.ceil(5 / 2));
  assert.equal(result.items.length, 2);
  // Sorted updatedAt descending (ids 5,4,3,2,1), so page 2 is ids 3 and 2.
  assert.deepEqual(result.items.map((r) => r.number), [3, 2]);
});

test('list: items are sorted by updatedAt descending', async () => {
  const calls = [];
  const raw = [
    makeRaw({ id: 10, updatedDate: 1792622035057 }),
    makeRaw({ id: 11, updatedDate: 1792622035057 + 60000 }),
    makeRaw({ id: 12, updatedDate: 1792622035057 - 60000 }),
  ];
  const client = fakeClient({ calls, pullRequestsImpl: async () => raw });
  const ref = { host: 'h', project: 'PROJ', repo: 'repo' };
  const result = await list({ client, query: {}, repos: [ref], page: 1, limit: 36 });
  assert.deepEqual(result.items.map((r) => r.number), [11, 10, 12]);
});
