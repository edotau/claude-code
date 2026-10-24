'use strict';
// Unit tests for lib/tasks-view.js's cell/menu contract: data-cell/data-hydrate attributes, rowCells(),
// the GitHub issue status menu and PR merge menu, assignees/reviewers action buttons, loading checks,
// row errors, and the hydrate script. Spec: AGENTS brief for vscode/claude-code-agents/lib/tasks-view.js.
const test = require('node:test');
const assert = require('node:assert/strict');
const { renderTasksHtml, rowCells } = require('../lib/tasks-view');
const { mapPullRequest, mapIssue } = require('../lib/tasks-github');
const { presetsFor } = require('../lib/tasks-state');

const REPO = { owner: 'acme', name: 'widgets' };
const opts = { nonce: 'n', cspSource: 'c' };

function baseModel(overrides) {
  return {
    sources: [{ id: 'github' }],
    source: 'github',
    scope: 'all',
    state: 'open',
    query: '',
    presets: presetsFor('github'),
    activePreset: null,
    defaultPreset: 'assigned',
    repos: [{ label: 'acme/widgets', selected: true }],
    repoCount: 1,
    banner: null,
    loading: false,
    loadingPage: null,
    items: [],
    errors: [],
    failedRepos: 0,
    hardError: null,
    page: 1,
    totalPages: 1,
    now: Date.parse('2026-10-24T12:00:00Z'),
    settings: { expanded: false, defaultSource: 'github', github: { status: { ok: true }, visible: true, repos: [] } },
    ...(overrides || {}),
  };
}

function unhydratedPr(overrides) {
  return mapPullRequest({ number: 1, title: 'Add thing', state: 'OPEN', url: 'https://github.com/acme/widgets/pull/1', updatedAt: '2026-10-24T00:00:00Z', ...(overrides || {}) }, REPO);
}

function hydratedPr(overrides) {
  return mapPullRequest({ number: 2, title: 'Fix thing', state: 'OPEN', url: 'https://github.com/acme/widgets/pull/2', updatedAt: '2026-10-24T00:00:00Z', statusCheckRollup: [], mergeable: 'MERGEABLE', mergeStateStatus: 'CLEAN', ...(overrides || {}) }, REPO);
}

function openIssue(overrides) {
  return mapIssue({ number: 3, title: 'Report bug', state: 'OPEN', url: 'https://github.com/acme/widgets/issues/3', updatedAt: '2026-10-24T00:00:00Z', author: { login: 'dev1' }, assignees: [], ...(overrides || {}) }, REPO);
}

// -- data-hydrate on un-hydrated GitHub PR rows only --

test('row: an un-hydrated GitHub PR row carries data-hydrate="1"', () => {
  const html = renderTasksHtml(baseModel({ scope: 'pr', items: [unhydratedPr()] }), opts);
  assert.match(html, /data-id="github:acme\/widgets:pr:1"[^>]*data-hydrate="1"/);
});

test('row: a hydrated GitHub PR row carries no data-hydrate attribute', () => {
  const html = renderTasksHtml(baseModel({ scope: 'pr', items: [hydratedPr()] }), opts);
  const rowHtml = html.slice(html.indexOf('data-id="github:acme/widgets:pr:2"') - 40, html.indexOf('data-id="github:acme/widgets:pr:2"') + 60);
  assert.ok(!rowHtml.includes('data-hydrate'));
});

test('row: an issue row never carries data-hydrate', () => {
  const html = renderTasksHtml(baseModel({ scope: 'issue', items: [openIssue()] }), opts);
  assert.ok(!html.includes('data-hydrate="1"'), 'only the hydrate script itself should mention data-hydrate, never a row attribute');
});

// -- data-cell on every cell --

test('rowCells/row: PR rows name every cell (id, title, reviewers, checks, merge, updated, actions)', () => {
  const item = hydratedPr();
  const model = baseModel({ scope: 'pr', items: [item] });
  const cells = rowCells(item, model, 'pr');
  assert.deepEqual(Object.keys(cells), ['id', 'title', 'reviewers', 'checks', 'merge', 'updated', 'actions']);
  const html = renderTasksHtml(model, opts);
  for (const name of Object.keys(cells)) assert.ok(html.includes(`data-cell="${name}"`), `missing data-cell="${name}"`);
});

test('rowCells/row: issue rows name id/title/assignees/status/updated/actions', () => {
  const item = openIssue();
  const model = baseModel({ scope: 'issue', items: [item] });
  const cells = rowCells(item, model, 'issue');
  assert.deepEqual(Object.keys(cells), ['id', 'title', 'assignees', 'status', 'updated', 'actions']);
});

test('rowCells matches what row() renders for the same item/model/kind', () => {
  const item = hydratedPr();
  const model = baseModel({ scope: 'pr', items: [item] });
  const cells = rowCells(item, model, 'pr');
  const html = renderTasksHtml(model, opts);
  for (const [name, cellHtml] of Object.entries(cells)) {
    assert.ok(html.includes(cellHtml) || cellHtml === '', `row() output diverges from rowCells() for "${name}"`);
  }
});

// -- GitHub issue Status menu --

test('issue Status cell is a menu with Open/Closed-reason items', () => {
  const item = openIssue();
  const html = rowCells(item, baseModel(), 'issue').status;
  assert.match(html, /<details class="menu">/);
  assert.match(html, /<summary class="pill-menu"[^>]*>.*Open.*<\/summary>/s);
  assert.match(html, /data-issue-state="open"/);
  assert.match(html, /data-issue-state="closed" data-reason="completed"[^>]*>.*Close as completed/s);
  assert.match(html, /data-issue-state="closed" data-reason="not_planned"[^>]*>.*Close as not planned/s);
  assert.match(html, /data-issue-state="closed" data-reason="duplicate"[^>]*>.*Close as duplicate…/s);
});

test('Bitbucket/Jira status cells are plain pills with no menu', () => {
  const bbItem = { source: 'bitbucket', type: 'pr', state: 'open', id: 'bb:1', repo: { owner: 'a', name: 'b', label: 'a/b' }, url: 'u' };
  assert.ok(!rowCells(bbItem, baseModel(), 'issue').status.includes('<details'));
  const jiraItem = { source: 'jira', type: 'issue', id: 'jira:1', repo: { owner: 'a', name: 'b', label: 'a/b' }, url: 'u', status: { name: 'In Progress', category: 'indeterminate' } };
  assert.ok(!rowCells(jiraItem, baseModel(), 'jira').status.includes('<details'));
});

// -- GitHub Merge menu (open/draft hydrated PR) --

test('Merge cell for an open hydrated PR is a menu with auto-merge, allowed methods, and the browser-open item', () => {
  const item = hydratedPr({ autoMergeRequest: null });
  const model = baseModel({ mergeMethods: { 'acme/widgets': { merge: false, squash: true, rebase: true } } });
  const html = rowCells(item, model, 'pr').merge;
  assert.match(html, /<details class="menu">/);
  assert.match(html, /data-auto-merge="enable"[^>]*>.*Enable auto-merge/s);
  assert.ok(!html.includes('Create a merge commit'), 'merge method disallowed by mergeMethods map is omitted');
  assert.ok(html.includes('Squash and merge'));
  assert.ok(html.includes('Rebase and merge'));
  assert.match(html, /data-url="[^"]*"[^>]*>.*Open GitHub merge box/s);
});

test('Merge cell shows "Disable auto-merge" when the PR already has auto-merge enabled', () => {
  const item = hydratedPr({ autoMergeRequest: { mergeMethod: 'SQUASH' } });
  const html = rowCells(item, baseModel(), 'pr').merge;
  assert.match(html, /data-auto-merge="disable"[^>]*>.*Disable auto-merge/s);
});

test('Merge cell shows all three methods when the repo has no mergeMethods entry', () => {
  const item = hydratedPr();
  const html = rowCells(item, baseModel(), 'pr').merge;
  for (const label of ['Create a merge commit', 'Squash and merge', 'Rebase and merge']) assert.ok(html.includes(label));
});

test('Merge cell for a loading (un-hydrated) PR renders a muted "…" and no menu', () => {
  const item = unhydratedPr();
  const html = rowCells(item, baseModel(), 'pr').merge;
  assert.ok(html.includes('…'));
  assert.ok(!html.includes('<details'));
});

test('Merge cell for a merged/closed PR renders the plain pill, not a menu', () => {
  const merged = mapPullRequest({ number: 9, title: 't', state: 'MERGED', mergedAt: 'x', url: 'u', updatedAt: 'x' }, REPO);
  const html = rowCells(merged, baseModel(), 'pr').merge;
  assert.ok(!html.includes('<details'));
  assert.ok(html.includes('Merged'));
});

// -- Assignees / Reviewers action buttons --

test('GitHub Assignees cell is a button wrapping the avatar stack', () => {
  const item = openIssue({ assignees: [{ login: 'dev2' }] });
  const html = rowCells(item, baseModel(), 'issue').assignees;
  assert.match(html, /<button class="cell-btn" data-act="assignees" data-id="[^"]+"/);
});

test('GitHub PR Reviewers cell is a button wrapping the reviewer chip', () => {
  const item = hydratedPr({ reviewRequests: [{ login: 'dev3' }] });
  const html = rowCells(item, baseModel(), 'pr').reviewers;
  assert.match(html, /<button class="cell-btn" data-act="reviewers" data-id="[^"]+"/);
});

test('Bitbucket reviewers stay a plain span, not a button', () => {
  const item = { source: 'bitbucket', type: 'pr', state: 'open', id: 'bb:5', repo: { owner: 'a', name: 'b', label: 'a/b' }, url: 'u', reviewers: [{ name: 'dev4', status: 'APPROVED' }] };
  const html = rowCells(item, baseModel(), 'pr').reviewers;
  assert.ok(!html.includes('data-act="reviewers"'));
  assert.match(html, /<span class="reviewers"/);
});

// -- Checks cell loading --

test('Checks cell renders a muted "…" while loading', () => {
  const item = unhydratedPr();
  const html = rowCells(item, baseModel(), 'pr').checks;
  assert.match(html, /<span class="muted cell-text">…<\/span>/);
});

// -- rowErrors --

test('model.rowErrors renders a row-error line with Retry and dismiss buttons in the title cell', () => {
  const item = openIssue();
  const model = baseModel({ rowErrors: { [item.id]: 'Failed to load reviewers' } });
  const html = rowCells(item, model, 'issue').title;
  assert.ok(html.includes('Failed to load reviewers'));
  assert.match(html, /data-act="retry" data-id="[^"]+"[^>]*>Retry/);
  assert.match(html, /data-act="clearError" data-id="[^"]+"/);
});

test('with no rowErrors entry for the item, the title cell has no row-error line', () => {
  const item = openIssue();
  const html = rowCells(item, baseModel({ rowErrors: {} }), 'issue').title;
  assert.ok(!html.includes('row-error'));
});

// -- hydrate script present in the page --

test('renderTasksHtml includes the IntersectionObserver hydrate script posting {type: "hydrate", ids}', () => {
  const html = renderTasksHtml(baseModel({ scope: 'pr', items: [unhydratedPr()] }), opts);
  assert.ok(html.includes('IntersectionObserver'));
  assert.ok(html.includes("'hydrate'"));
});
