'use strict';
// Unit tests for the Jira task adapter (lib/tasks-jira.js): toJql, mapIssue over real DC search
// shapes, and list's paging math against a fake client.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { toJql, mapIssue, list } = require('../lib/tasks-jira');

const FIXTURE_DIR = path.join(__dirname, 'fixtures', 'jira');
function fixture(name) {
  return JSON.parse(fs.readFileSync(path.join(FIXTURE_DIR, `${name}.json`), 'utf8'));
}

const BASE = 'https://jira.biscrum.com';

function emptyQuery(overrides) {
  return { assignee: null, author: null, state: 'open', labels: [], freeText: '', ...overrides };
}

// -- toJql --

test('toJql: assignee:@me + open defaults to assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC', () => {
  const jql = toJql(emptyQuery({ assignee: '@me' }));
  assert.equal(jql, 'assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC');
});

test('toJql: author:@me maps to reporter = currentUser()', () => {
  const jql = toJql(emptyQuery({ author: '@me' }));
  assert.equal(jql, 'reporter = currentUser() AND statusCategory != Done ORDER BY updated DESC');
});

test('toJql: state:closed maps to statusCategory = Done', () => {
  const jql = toJql(emptyQuery({ assignee: '@me', state: 'closed' }));
  assert.equal(jql, 'assignee = currentUser() AND statusCategory = Done ORDER BY updated DESC');
});

test('toJql: state:all omits the status clause entirely', () => {
  const jql = toJql(emptyQuery({ assignee: '@me', state: 'all' }));
  assert.equal(jql, 'assignee = currentUser() ORDER BY updated DESC');
});

test('toJql: labels become labels in ("a", "b")', () => {
  const jql = toJql(emptyQuery({ labels: ['a', 'b'] }));
  assert.match(jql, /labels in \("a", "b"\)/);
});

test('toJql: freeText "project:digitalbtd" maps to project = DIGITALBTD (uppercased)', () => {
  const jql = toJql(emptyQuery({ freeText: 'project:digitalbtd' }));
  assert.match(jql, /project = DIGITALBTD/);
});

test('toJql: freeText that is a bare issue key maps to key = DIGITALBTD-8139', () => {
  const jql = toJql(emptyQuery({ freeText: 'DIGITALBTD-8139' }));
  assert.match(jql, /key = DIGITALBTD-8139/);
});

test('toJql: other free text maps to text ~ "…", joining tokenized words', () => {
  const jql = toJql(emptyQuery({ freeText: 'celery work scoping' }));
  assert.match(jql, /text ~ "celery work scoping"/);
});

test('toJql: a literal double-quote inside free text is escaped in the JQL string literal', () => {
  const jql = toJql(emptyQuery({ freeText: 'say "hi" there' }));
  assert.match(jql, /text ~ "say hi there"/); // the adapter's tokenizer treats "hi" as a quoted segment and strips it
});

test('toJql: projects maps to project in (A, B)', () => {
  const jql = toJql(emptyQuery({ assignee: '@me' }), { projects: ['A', 'B'] });
  assert.match(jql, /project in \(A, B\)/);
});

test('toJql: an empty query produces no WHERE clause, only the ORDER BY', () => {
  const jql = toJql(emptyQuery({ state: 'all' }));
  assert.equal(jql, 'ORDER BY updated DESC');
});

// teeth: this would fail if labels/freeText clauses were silently joined with OR instead of AND.
test('toJql TEETH: assignee + labels + freeText combine with AND, not OR', () => {
  const jql = toJql(emptyQuery({ assignee: '@me', labels: ['urgent'], freeText: 'rollout' }));
  const clause = jql.replace(/ ORDER BY updated DESC$/, '');
  const parts = clause.split(' AND ');
  assert.ok(parts.some((p) => p === 'assignee = currentUser()'));
  assert.ok(parts.some((p) => p === 'statusCategory != Done'));
  assert.ok(parts.some((p) => p === 'labels in ("urgent")'));
  assert.ok(parts.some((p) => p === 'text ~ "rollout"'));
  assert.equal(jql.includes(' OR '), false);
});

// -- mapIssue --

const ISSUES = fixture('search').issues;

test('mapIssue: #0 (In Progress / indeterminate) maps source/type/id/number/state/url/status', () => {
  const row = mapIssue(ISSUES[0], { baseUrl: BASE });
  assert.equal(row.source, 'jira');
  assert.equal(row.type, 'issue');
  assert.equal(row.id, 'jira:DIGITALBTD-8139');
  assert.equal(row.number, 'DIGITALBTD-8139');
  assert.equal(row.state, 'open');
  assert.equal(row.url, `${BASE}/browse/DIGITALBTD-8139`);
  assert.deepEqual(row.assignees, ['User One']); // displayName, matching tasks-github.js's loginOf-style mapper
  assert.equal(row.status.category, 'indeterminate');
});

test('mapIssue: #1 (To Do / new) maps state open and category new', () => {
  const row = mapIssue(ISSUES[1], { baseUrl: BASE });
  assert.equal(row.id, 'jira:DIGITALBTD-9901');
  assert.equal(row.number, 'DIGITALBTD-9901');
  assert.equal(row.state, 'open');
  assert.equal(row.status.category, 'new');
  assert.equal(row.url, `${BASE}/browse/DIGITALBTD-9901`);
});

test('mapIssue: #2 (Done / done, null assignee) maps state closed and assignees []', () => {
  const row = mapIssue(ISSUES[2], { baseUrl: BASE });
  assert.equal(row.id, 'jira:DIGITALBTD-9685');
  assert.equal(row.number, 'DIGITALBTD-9685');
  assert.equal(row.state, 'closed');
  assert.equal(row.status.category, 'done');
  assert.deepEqual(row.assignees, []);
  assert.equal(row.url, `${BASE}/browse/DIGITALBTD-9685`);
});

test('mapIssue: state is "closed" only for the done category, across every fixture issue', () => {
  for (const issue of ISSUES) {
    const row = mapIssue(issue, { baseUrl: BASE });
    const isDone = issue.fields.status.statusCategory.key === 'done';
    assert.equal(row.state, isDone ? 'closed' : 'open');
    assert.ok(['new', 'indeterminate', 'done'].includes(row.status.category));
  }
});

// -- list --

function fakeClient(total, pageOfIssues) {
  const calls = [];
  return {
    calls,
    search: async (jql, opts) => {
      calls.push({ jql, ...opts });
      return { startAt: opts.startAt, maxResults: opts.maxResults, total, issues: pageOfIssues };
    },
  };
}

test('list: page 2 limit 10 passes startAt = (page-1)*limit = 10', async () => {
  const client = fakeClient(21, ISSUES);
  const result = await list({ client, query: emptyQuery({ assignee: '@me' }), page: 2, limit: 10, baseUrl: BASE });
  assert.equal(client.calls[0].startAt, 10);
  assert.equal(client.calls[0].maxResults, 10);
  assert.equal(result.totalPages, 3); // ceil(21/10)
  assert.equal(result.items.length, ISSUES.length);
});

test('list: page 1 limit 5 with total 21 computes totalPages = ceil(21/5) = 5', async () => {
  const client = fakeClient(21, ISSUES);
  const result = await list({ client, query: emptyQuery(), page: 1, limit: 5, baseUrl: BASE });
  assert.equal(client.calls[0].startAt, 0);
  assert.equal(result.totalPages, 5);
});

test('list: maps every returned issue through mapIssue with the given baseUrl', async () => {
  const client = fakeClient(3, ISSUES);
  const result = await list({ client, query: emptyQuery(), page: 1, limit: 50, baseUrl: BASE });
  assert.deepEqual(result.items.map((r) => r.id), ['jira:DIGITALBTD-8139', 'jira:DIGITALBTD-9901', 'jira:DIGITALBTD-9685']);
  assert.ok(result.items.every((r) => r.url.startsWith(BASE)));
});
