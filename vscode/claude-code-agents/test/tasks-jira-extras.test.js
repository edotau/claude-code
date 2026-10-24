'use strict';
// Unit tests for the Jira "extras": lib/jira-client.js boardStatusOrder/favouriteFilters, and
// lib/tasks-jira.js sort/currentSprint/statuses on toJql, isRawJql/rawJql, list's raw-JQL branch,
// statusCatalog, groupByStatus.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { createClient } = require('../lib/jira-client');
const { toJql, isRawJql, rawJql, list, statusCatalog, groupByStatus, mapIssue } = require('../lib/tasks-jira');
const { jiraColumns } = require('../lib/tasks-board');

const FIXTURE_DIR = path.join(__dirname, 'fixtures', 'jira');
function fixture(name) {
  return JSON.parse(fs.readFileSync(path.join(FIXTURE_DIR, `${name}.json`), 'utf8'));
}

function jsonResponse(status, body) {
  if (status === 204) return new Response(null, { status: 204 });
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
}

function fakeFetch(routes) {
  const calls = [];
  const fetchImpl = async (url, init) => {
    calls.push({ url, init });
    for (const [matcher, respond] of routes) {
      const parsed = new URL(url);
      if (matcher(parsed, init)) return respond(parsed, init);
    }
    throw new Error(`fakeFetch: no route for ${url}`);
  };
  return { fetchImpl, calls };
}

const BASE = 'https://jira.biscrum.com';
const TOKEN = 'T';

function emptyQuery(overrides) {
  return { assignee: null, author: null, state: 'open', labels: [], freeText: '', ...overrides };
}

// ===== lib/jira-client.js: boardStatusOrder =====

test('boardStatusOrder: GET /rest/agile/1.0/board?projectKeyOrId=KEY&maxResults=2, one board -> fetches its configuration', async () => {
  const boardList = { values: [{ id: 42, name: 'DIGITALBTD board' }] };
  const config = {
    id: 42,
    columnConfig: {
      columns: [
        { name: 'To Do', statuses: [{ id: '1' }] },
        { name: 'In Progress', statuses: [{ id: '2' }, { id: '3' }] },
        { name: 'Done', statuses: [{ id: '4' }] },
      ],
    },
  };
  const { fetchImpl, calls } = fakeFetch([
    [(u) => u.pathname === '/rest/agile/1.0/board', () => jsonResponse(200, boardList)],
    [(u) => u.pathname === '/rest/agile/1.0/board/42/configuration', () => jsonResponse(200, config)],
  ]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  const result = await client.boardStatusOrder('DIGITALBTD');

  assert.equal(calls.length, 2);
  const listUrl = new URL(calls[0].url);
  assert.equal(listUrl.pathname, '/rest/agile/1.0/board');
  assert.equal(listUrl.searchParams.get('projectKeyOrId'), 'DIGITALBTD');
  assert.equal(listUrl.searchParams.get('maxResults'), '2');
  const confUrl = new URL(calls[1].url);
  assert.equal(confUrl.pathname, '/rest/agile/1.0/board/42/configuration');

  assert.deepEqual(result, {
    boardId: '42',
    columns: [
      { name: 'To Do', statusIds: ['1'] },
      { name: 'In Progress', statusIds: ['2', '3'] },
      { name: 'Done', statusIds: ['4'] },
    ],
  });
});

test('boardStatusOrder: zero boards resolves null (no second request)', async () => {
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(200, { values: [] })]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  const result = await client.boardStatusOrder('DIGITALBTD');
  assert.equal(result, null);
  assert.equal(calls.length, 1);
});

test('boardStatusOrder: several boards resolves null (no second request)', async () => {
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(200, { values: [{ id: 1 }, { id: 2 }] })]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  const result = await client.boardStatusOrder('DIGITALBTD');
  assert.equal(result, null);
  assert.equal(calls.length, 1);
});

test('boardStatusOrder: an HTTP error on the board list still throws (not swallowed into null)', async () => {
  const { fetchImpl } = fakeFetch([[() => true, () => jsonResponse(400, { errorMessages: ['bad project'] })]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  await assert.rejects(() => client.boardStatusOrder('NOPE'), (err) => {
    assert.equal(err.status, 400);
    return true;
  });
});

// ===== lib/jira-client.js: favouriteFilters =====

test('favouriteFilters: GET /rest/api/2/filter/favourite?expand=jql, mapped to {id, name, jql}', async () => {
  const body = [
    { id: '10021', name: 'My open stories', jql: 'assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC', self: 'x', favourite: true },
    { id: '10022', name: 'Sprint work', jql: 'sprint in openSprints() ORDER BY updated DESC', self: 'y', favourite: true },
  ];
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(200, body)]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  const result = await client.favouriteFilters();

  const url = new URL(calls[0].url);
  assert.equal(url.pathname, '/rest/api/2/filter/favourite');
  assert.equal(url.searchParams.get('expand'), 'jql');
  assert.deepEqual(result, [
    { id: '10021', name: 'My open stories', jql: 'assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC' },
    { id: '10022', name: 'Sprint work', jql: 'sprint in openSprints() ORDER BY updated DESC' },
  ]);
});

test('favouriteFilters: an empty list resolves []', async () => {
  const { fetchImpl } = fakeFetch([[() => true, () => jsonResponse(200, [])]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  const result = await client.favouriteFilters();
  assert.deepEqual(result, []);
});

// ===== lib/tasks-jira.js: toJql sort / currentSprint / statuses =====

test('toJql: opts.sort overrides the default ORDER BY updated DESC', () => {
  const jql = toJql(emptyQuery({ assignee: '@me' }), { sort: { field: 'assignee', dir: 'asc' } });
  assert.equal(jql, 'assignee = currentUser() AND statusCategory != Done ORDER BY assignee ASC');
});

test('toJql: opts.sort field "title" maps to the summary column', () => {
  const jql = toJql(emptyQuery({ state: 'all' }), { sort: { field: 'title', dir: 'desc' } });
  assert.equal(jql, 'ORDER BY summary DESC');
});

for (const [field, column] of [['key', 'key'], ['status', 'status'], ['priority', 'priority'], ['assignee', 'assignee'], ['updated', 'updated'], ['created', 'created']]) {
  test(`toJql: opts.sort field "${field}" maps to column "${column}"`, () => {
    const jql = toJql(emptyQuery({ state: 'all' }), { sort: { field, dir: 'asc' } });
    assert.equal(jql, `ORDER BY ${column} ASC`);
  });
}

test('toJql: opts.currentSprint adds "sprint in openSprints()", AND-ed before ORDER BY', () => {
  const jql = toJql(emptyQuery({ assignee: '@me' }), { currentSprint: true });
  assert.equal(jql, 'assignee = currentUser() AND statusCategory != Done AND sprint in openSprints() ORDER BY updated DESC');
});

test('toJql: currentSprint false (or absent) adds no sprint clause', () => {
  const jql = toJql(emptyQuery({ assignee: '@me' }), { currentSprint: false });
  assert.equal(jql, 'assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC');
});

test('toJql: opts.statuses adds status in ("A", "B"), AND-ed before ORDER BY', () => {
  const jql = toJql(emptyQuery({ state: 'all' }), { statuses: ['To Do', 'In Progress'] });
  assert.equal(jql, 'status in ("To Do", "In Progress") ORDER BY updated DESC');
});

test('toJql TEETH: a status name containing a double quote is escaped in the JQL', () => {
  const jql = toJql(emptyQuery({ state: 'all' }), { statuses: ['Needs "Review"'] });
  assert.equal(jql, 'status in ("Needs \\"Review\\"") ORDER BY updated DESC');
});

test('toJql: sort + currentSprint + statuses combine with AND, ORDER BY from sort wins', () => {
  const jql = toJql(emptyQuery({ assignee: '@me' }), {
    sort: { field: 'priority', dir: 'desc' },
    currentSprint: true,
    statuses: ['Done'],
  });
  assert.equal(
    jql,
    'assignee = currentUser() AND statusCategory != Done AND status in ("Done") AND sprint in openSprints() ORDER BY priority DESC'
  );
});

// ===== lib/tasks-jira.js: isRawJql =====

test('isRawJql: a "jql:" prefix (any case) is raw', () => {
  assert.equal(isRawJql('jql:project = ABC'), true);
  assert.equal(isRawJql('JQL: project = ABC'), true);
  assert.equal(isRawJql('Jql:project = ABC'), true);
});

for (const text of [
  'project = DIGITALBTD',
  'status != Blocked',
  'labels in ("a", "b")',
  'summary ~ "rollout"',
  'resolution is empty',
  'resolution is null',
  'project = DIGITALBTD order by updated desc',
  'ORDER BY updated DESC',
]) {
  test(`isRawJql: Orca's heuristic matches ${JSON.stringify(text)}`, () => {
    assert.equal(isRawJql(text), true);
  });
}

for (const text of ['hello world', 'project:KEY', 'status:Blocked', 'DIGITALBTD-8139', '', 'celery work scoping']) {
  test(`isRawJql: plain text is not raw: ${JSON.stringify(text)}`, () => {
    assert.equal(isRawJql(text), false);
  });
}

// ===== lib/tasks-jira.js: rawJql =====

test('rawJql: strips a "jql:" prefix and appends ORDER BY updated DESC when the text has none', () => {
  const jql = rawJql('jql:project = DIGITALBTD');
  assert.equal(jql, 'project = DIGITALBTD ORDER BY updated DESC');
});

test('rawJql: a "jql:" prefix is stripped case-insensitively', () => {
  const jql = rawJql('JQL: project = DIGITALBTD');
  assert.equal(jql, 'project = DIGITALBTD ORDER BY updated DESC');
});

test('rawJql: text that already has an ORDER BY is left as-is when no opts are given', () => {
  const jql = rawJql('project = DIGITALBTD order by created desc');
  assert.equal(jql, 'project = DIGITALBTD order by created desc');
});

test('rawJql: opts.sort replaces an existing ORDER BY', () => {
  const jql = rawJql('project = DIGITALBTD order by created desc', { sort: { field: 'updated', dir: 'asc' } });
  assert.equal(jql, 'project = DIGITALBTD ORDER BY updated ASC');
});

test('rawJql: opts.sort sets the ORDER BY when the text has none', () => {
  const jql = rawJql('project = DIGITALBTD', { sort: { field: 'key', dir: 'desc' } });
  assert.equal(jql, 'project = DIGITALBTD ORDER BY key DESC');
});

test('rawJql: opts.currentSprint and opts.statuses AND in before the ORDER BY, wrapping the original body in parens', () => {
  const jql = rawJql('project = DIGITALBTD', { currentSprint: true, statuses: ['Done'] });
  assert.equal(jql, '(project = DIGITALBTD) AND status in ("Done") AND sprint in openSprints() ORDER BY updated DESC');
});

test('rawJql TEETH: a status name containing a double quote is escaped', () => {
  const jql = rawJql('project = DIGITALBTD', { statuses: ['Needs "Review"'] });
  assert.equal(jql, '(project = DIGITALBTD) AND status in ("Needs \\"Review\\"") ORDER BY updated DESC');
});

test('rawJql: with no original body text, extras are AND-ed without parens', () => {
  const jql = rawJql('', { statuses: ['Done'] });
  assert.equal(jql, 'status in ("Done") ORDER BY updated DESC');
});

// ===== lib/tasks-jira.js: list — raw vs parsed JQL =====

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

const ISSUES = fixture('search').issues;

test('list: a raw-JQL rawQuery ("jql:…") goes through rawJql, not toJql', async () => {
  const client = fakeClient(1, [ISSUES[0]]);
  await list({ client, query: emptyQuery(), rawQuery: 'jql:project = DIGITALBTD', page: 1, limit: 10, baseUrl: BASE });
  assert.equal(client.calls[0].jql, 'project = DIGITALBTD ORDER BY updated DESC');
});

test('list: a raw-JQL rawQuery honours sort/currentSprint/statuses', async () => {
  const client = fakeClient(1, [ISSUES[0]]);
  await list({
    client,
    query: emptyQuery(),
    rawQuery: 'jql:project = DIGITALBTD',
    sort: { field: 'priority', dir: 'asc' },
    currentSprint: true,
    page: 1,
    limit: 10,
    baseUrl: BASE,
  });
  assert.equal(client.calls[0].jql, '(project = DIGITALBTD) AND sprint in openSprints() ORDER BY priority ASC');
});

test('list: a non-raw rawQuery (or none) falls back to toJql(query, …)', async () => {
  const client = fakeClient(1, [ISSUES[0]]);
  await list({ client, query: emptyQuery({ assignee: '@me' }), page: 1, limit: 10, baseUrl: BASE });
  assert.equal(client.calls[0].jql, toJql(emptyQuery({ assignee: '@me' }), {}));
});

test('list: toJql path also honours sort/currentSprint/statuses', async () => {
  const client = fakeClient(1, [ISSUES[0]]);
  await list({
    client,
    query: emptyQuery({ assignee: '@me' }),
    statuses: ['To Do'],
    page: 1,
    limit: 10,
    baseUrl: BASE,
  });
  assert.equal(
    client.calls[0].jql,
    toJql(emptyQuery({ assignee: '@me' }), { statuses: ['To Do'] })
  );
});

// ===== lib/tasks-jira.js: statusCatalog =====

test('statusCatalog: groups rows by status name with counts, ordered To Do · In Progress · Done then name', () => {
  const rows = [
    mapIssue(ISSUES[1], { baseUrl: BASE }), // To Do / new
    mapIssue(ISSUES[0], { baseUrl: BASE }), // In Progress / indeterminate
    mapIssue(ISSUES[2], { baseUrl: BASE }), // Done / done
    mapIssue(ISSUES[1], { baseUrl: BASE }), // another To Do
  ];
  const catalog = statusCatalog(rows);
  assert.deepEqual(catalog, [
    { name: 'To Do', category: 'new', count: 2 },
    { name: 'In Progress', category: 'indeterminate', count: 1 },
    { name: 'Done', category: 'done', count: 1 },
  ]);
});

test('statusCatalog: within the same category, names sort alphabetically', () => {
  const rowA = { ...mapIssue(ISSUES[1], { baseUrl: BASE }), status: { name: 'Backlog', category: 'new' } };
  const rowB = { ...mapIssue(ISSUES[1], { baseUrl: BASE }), status: { name: 'Analysis', category: 'new' } };
  const catalog = statusCatalog([rowA, rowB]);
  assert.deepEqual(catalog.map((c) => c.name), ['Analysis', 'Backlog']);
});

test('statusCatalog: an empty row list yields an empty catalog', () => {
  assert.deepEqual(statusCatalog([]), []);
});

// ===== lib/tasks-jira.js: groupByStatus =====

test('groupByStatus: delegates to tasks-board.jiraColumns(rows, order)', () => {
  const rows = [mapIssue(ISSUES[0], { baseUrl: BASE }), mapIssue(ISSUES[1], { baseUrl: BASE }), mapIssue(ISSUES[2], { baseUrl: BASE })];
  const order = ['To Do', 'In Progress', 'Done'];
  assert.deepEqual(groupByStatus(rows, order), jiraColumns(rows, order));
});

test('groupByStatus: respects the given column order over the category fallback', () => {
  const rows = [mapIssue(ISSUES[0], { baseUrl: BASE }), mapIssue(ISSUES[1], { baseUrl: BASE }), mapIssue(ISSUES[2], { baseUrl: BASE })];
  const order = ['Done', 'In Progress', 'To Do'];
  const groups = groupByStatus(rows, order);
  assert.deepEqual(groups.map((g) => g.label), ['Done', 'In Progress', 'To Do']);
});
